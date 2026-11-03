package ca

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/cloudflare/cfssl/api"
	"github.com/cloudflare/cfssl/config"
	"github.com/cloudflare/cfssl/csr"
	"github.com/cloudflare/cfssl/signer"
	"github.com/moby/swarmkit/v2/log"
	"github.com/pkg/errors"
)

const (
	// ExternalCrossSignProfile is the profile that we will be sending cross-signing CSR sign requests with
	ExternalCrossSignProfile = "CA"

	// CertificateMaxSize is the maximum expected size of a certificate.
	// While there is no specced upper limit to the size of an x509 certificate in PEM format,
	// one with a ridiculous RSA key size (16384) and 26 256-character DNS SAN fields is about 14k.
	// While there is no upper limit on the length of certificate chains, long chains are impractical.
	// To be conservative, and to also account for external CA certificate responses in JSON format
	// from CFSSL, we'll set the max to be 256KiB.
	CertificateMaxSize int64 = 256 << 10
)

// ErrNoExternalCAURLs is an error used it indicate that an ExternalCA is
// configured with no URLs to which it can proxy certificate signing requests.
var ErrNoExternalCAURLs = errors.New("no external CA URLs")

// ExternalCA is able to make certificate signing requests to one of a list
// remote CFSSL API endpoints.
type ExternalCA struct {
	ExternalRequestTimeout time.Duration

	mu            sync.Mutex
	intermediates []byte
	urls          []string
	client        *http.Client
}

// NewExternalCATLSConfig takes a TLS certificate and root pool and returns a TLS config that can be updated
// without killing existing connections
func NewExternalCATLSConfig(certs []tls.Certificate, rootPool *x509.CertPool) *tls.Config {
	return &tls.Config{
		Certificates: certs,
		RootCAs:      rootPool,
		MinVersion:   tls.VersionTLS12,
	}
}

// NewExternalCA creates a new ExternalCA which uses the given tlsConfig to
// authenticate to any of the given URLS of CFSSL API endpoints.
func NewExternalCA(intermediates []byte, tlsConfig *tls.Config, urls ...string) *ExternalCA {
	return &ExternalCA{
		ExternalRequestTimeout: 5 * time.Second,
		intermediates:          intermediates,
		urls:                   urls,
		client: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: tlsConfig,
			},
		},
	}
}

// UpdateTLSConfig updates the HTTP Client for this ExternalCA by creating
// a new client which uses the given tlsConfig.
func (eca *ExternalCA) UpdateTLSConfig(tlsConfig *tls.Config) {
	eca.mu.Lock()
	defer eca.mu.Unlock()

	eca.client = &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: tlsConfig,
		},
	}
}

// UpdateURLs updates the list of CSR API endpoints by setting it to the given urls.
func (eca *ExternalCA) UpdateURLs(urls ...string) {
	eca.mu.Lock()
	defer eca.mu.Unlock()

	eca.urls = urls
}

// Sign signs a new certificate by proxying the given certificate signing
// request to an external CFSSL API server.
func (eca *ExternalCA) Sign(ctx context.Context, req signer.SignRequest) (cert []byte, err error) {
	// Get the current HTTP client and list of URLs in a small critical
	// section. We will use these to make certificate signing requests.
	eca.mu.Lock()
	urls := eca.urls
	client := eca.client
	intermediates := eca.intermediates
	eca.mu.Unlock()

	if len(urls) == 0 {
		return nil, ErrNoExternalCAURLs
	}

	csrJSON, err := json.Marshal(req)
	if err != nil {
		return nil, errors.Wrap(err, "unable to JSON-encode CFSSL signing request")
	}

	// Try each configured proxy URL. Return after the first success. If
	// all fail then the last error will be returned.
	for _, url := range urls {
		requestCtx, cancel := context.WithTimeout(ctx, eca.ExternalRequestTimeout)
		cert, err = makeExternalSignRequest(requestCtx, client, url, csrJSON)
		cancel()
		if err == nil {
			return append(cert, intermediates...), err
		}
		log.G(ctx).Debugf("unable to proxy certificate signing request to %s: %s", url, err)
	}

	return nil, err
}

// CrossSignRootCA takes a RootCA object, generates a CA CSR, sends a signing request with the CA CSR to the external
// CFSSL API server in order to obtain a cross-signed root
func (eca *ExternalCA) CrossSignRootCA(ctx context.Context, rca RootCA) ([]byte, error) {
	// ExtractCertificateRequest generates a new key request, and we want to continue to use the old
	// key.  However, ExtractCertificateRequest will also convert the pkix.Name to csr.Name, which we
	// need in order to generate a signing request
	rcaSigner, err := rca.Signer()
	if err != nil {
		return nil, err
	}
	rootCert := rcaSigner.parsedCert
	cfCSRObj := csr.ExtractCertificateRequest(rootCert)

	der, err := x509.CreateCertificateRequest(cryptorand.Reader, &x509.CertificateRequest{
		RawSubjectPublicKeyInfo: rootCert.RawSubjectPublicKeyInfo,
		RawSubject:              rootCert.RawSubject,
		PublicKeyAlgorithm:      rootCert.PublicKeyAlgorithm,
		Subject:                 rootCert.Subject,
		Extensions:              rootCert.Extensions,
		DNSNames:                rootCert.DNSNames,
		EmailAddresses:          rootCert.EmailAddresses,
		IPAddresses:             rootCert.IPAddresses,
	}, rcaSigner.cryptoSigner)
	if err != nil {
		return nil, err
	}
	req := signer.SignRequest{
		Request: string(pem.EncodeToMemory(&pem.Block{
			Type:  "CERTIFICATE REQUEST",
			Bytes: der,
		})),
		Subject: &signer.Subject{
			CN:    rootCert.Subject.CommonName,
			Names: cfCSRObj.Names,
		},
		Profile: ExternalCrossSignProfile,
	}
	// cfssl actually ignores non subject alt name extensions in the CSR, so we have to add the CA extension in the signing
	// request as well
	for _, ext := range rootCert.Extensions {
		if ext.Id.Equal(BasicConstraintsOID) {
			req.Extensions = append(req.Extensions, signer.Extension{
				ID:       config.OID(ext.Id),
				Critical: ext.Critical,
				Value:    hex.EncodeToString(ext.Value),
			})
		}
	}
	return eca.Sign(ctx, req)
}

func makeExternalSignRequest(ctx context.Context, client *http.Client, url string, csrJSON []byte) (cert []byte, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(csrJSON))
	if err != nil {
		return nil, recoverableErr{err: errors.Wrap(err, "unable to create certificate signing request")}
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, recoverableErr{err: errors.Wrap(err, "unable to perform certificate signing request")}
	}
	defer func() { _ = resp.Body.Close() }()

	b := io.LimitReader(resp.Body, CertificateMaxSize)
	body, err := io.ReadAll(b)
	if err != nil {
		return nil, recoverableErr{err: errors.Wrap(err, "unable to read CSR response body")}
	}

	if resp.StatusCode != http.StatusOK {
		return nil, recoverableErr{err: errors.Errorf("unexpected status code in CSR response: %d - %s", resp.StatusCode, string(body))}
	}

	var apiResponse api.Response
	if err := json.Unmarshal(body, &apiResponse); err != nil {
		log.G(ctx).Debugf("unable to JSON-parse CFSSL API response body: %s", string(body))
		return nil, recoverableErr{err: errors.Wrap(err, "unable to parse JSON response")}
	}

	if !apiResponse.Success || apiResponse.Result == nil {
		if len(apiResponse.Errors) > 0 {
			return nil, errors.Errorf("response errors: %v", apiResponse.Errors)
		}

		return nil, errors.New("certificate signing request failed")
	}

	result, ok := apiResponse.Result.(map[string]any)
	if !ok {
		return nil, errors.Errorf("invalid result type: %T", apiResponse.Result)
	}

	certPEM, ok := result["certificate"].(string)
	if !ok {
		return nil, errors.Errorf("invalid result certificate field type: %T", result["certificate"])
	}

	return []byte(certPEM), nil
}
