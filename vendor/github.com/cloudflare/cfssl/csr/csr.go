// Package csr implements certificate requests for CFSSL.
package csr

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/mail"
	"net/url"
	"strconv"
	"strings"

	cferr "github.com/cloudflare/cfssl/errors"
	"github.com/cloudflare/cfssl/helpers"
	"github.com/cloudflare/cfssl/helpers/derhelpers"
	"github.com/cloudflare/cfssl/log"
)

const (
	curveP256 = 256
	curveP384 = 384
	curveP521 = 521
)

// A Name contains the SubjectInfo fields.
type Name struct {
	C            string            `json:"C,omitempty" yaml:"C,omitempty"`   // Country
	ST           string            `json:"ST,omitempty" yaml:"ST,omitempty"` // State
	L            string            `json:"L,omitempty" yaml:"L,omitempty"`   // Locality
	O            string            `json:"O,omitempty" yaml:"O,omitempty"`   // OrganisationName
	OU           string            `json:"OU,omitempty" yaml:"OU,omitempty"` // OrganisationalUnitName
	E            string            `json:"E,omitempty" yaml:"E,omitempty"`
	SerialNumber string            `json:"SerialNumber,omitempty" yaml:"SerialNumber,omitempty"`
	OID          map[string]string `json:"OID,omitempty", yaml:"OID,omitempty"`
}

// A KeyRequest contains the algorithm and key size for a new private key.
type KeyRequest struct {
	A string `json:"algo" yaml:"algo"`
	S int    `json:"size" yaml:"size"`
}

// NewKeyRequest returns a default KeyRequest.
func NewKeyRequest() *KeyRequest {
	return &KeyRequest{"ecdsa", curveP256}
}

// Algo returns the requested key algorithm represented as a string.
func (kr *KeyRequest) Algo() string {
	return kr.A
}

// Size returns the requested key size.
func (kr *KeyRequest) Size() int {
	return kr.S
}

// Generate generates a key as specified in the request. Currently,
// ECDSA, RSA, Ed25519, and ML-DSA (mldsa44, mldsa65, mldsa87) algorithms are supported.
func (kr *KeyRequest) Generate() (crypto.PrivateKey, error) {
	log.Debugf("generate key from request: algo=%s, size=%d", kr.Algo(), kr.Size())
	switch kr.Algo() {
	case "rsa":
		if kr.Size() < 2048 {
			return nil, errors.New("RSA key is too weak")
		}
		if kr.Size() > 8192 {
			return nil, errors.New("RSA key size too large")
		}
		return rsa.GenerateKey(rand.Reader, kr.Size())
	case "ecdsa":
		var curve elliptic.Curve
		switch kr.Size() {
		case curveP256:
			curve = elliptic.P256()
		case curveP384:
			curve = elliptic.P384()
		case curveP521:
			curve = elliptic.P521()
		default:
			return nil, errors.New("invalid curve")
		}
		return ecdsa.GenerateKey(curve, rand.Reader)
	case "ed25519":
		seed := make([]byte, ed25519.SeedSize)
		if _, err := io.ReadFull(rand.Reader, seed); err != nil {
			return nil, err
		}
		return ed25519.NewKeyFromSeed(seed), nil
	case "mldsa44":
		return mldsa.GenerateKey(mldsa.MLDSA44())
	case "mldsa65":
		return mldsa.GenerateKey(mldsa.MLDSA65())
	case "mldsa87":
		return mldsa.GenerateKey(mldsa.MLDSA87())
	default:
		return nil, errors.New("invalid algorithm")
	}
}

// SigAlgo returns an appropriate X.509 signature algorithm given the
// key request's type and size.
func (kr *KeyRequest) SigAlgo() x509.SignatureAlgorithm {
	switch kr.Algo() {
	case "rsa":
		switch {
		case kr.Size() >= 4096:
			return x509.SHA512WithRSA
		case kr.Size() >= 3072:
			return x509.SHA384WithRSA
		case kr.Size() >= 2048:
			return x509.SHA256WithRSA
		default:
			return x509.SHA1WithRSA
		}
	case "ecdsa":
		switch kr.Size() {
		case curveP521:
			return x509.ECDSAWithSHA512
		case curveP384:
			return x509.ECDSAWithSHA384
		case curveP256:
			return x509.ECDSAWithSHA256
		default:
			return x509.ECDSAWithSHA1
		}
	case "ed25519":
		return x509.PureEd25519
	case "mldsa44":
		return x509.MLDSA44
	case "mldsa65":
		return x509.MLDSA65
	case "mldsa87":
		return x509.MLDSA87
	default:
		return x509.UnknownSignatureAlgorithm
	}
}

// CAConfig is a section used in the requests initialising a new CA.
type CAConfig struct {
	PathLength  int    `json:"pathlen" yaml:"pathlen"`
	PathLenZero bool   `json:"pathlenzero" yaml:"pathlenzero"`
	Expiry      string `json:"expiry" yaml:"expiry"`
	Backdate    string `json:"backdate" yaml:"backdate"`
}

// A CertificateRequest encapsulates the API interface to the
// certificate request functionality.
type CertificateRequest struct {
	CN                string           `json:"CN" yaml:"CN"`
	Names             []Name           `json:"names" yaml:"names"`
	Hosts             []string         `json:"hosts" yaml:"hosts"`
	KeyRequest        *KeyRequest      `json:"key,omitempty" yaml:"key,omitempty"`
	CA                *CAConfig        `json:"ca,omitempty" yaml:"ca,omitempty"`
	SerialNumber      string           `json:"serialnumber,omitempty" yaml:"serialnumber,omitempty"`
	DelegationEnabled bool             `json:"delegation_enabled,omitempty" yaml:"delegation_enabled,omitempty"`
	Extensions        []pkix.Extension `json:"extensions,omitempty" yaml:"extensions,omitempty"`
	CRL               string           `json:"crl_url,omitempty" yaml:"crl_url,omitempty"`
}

// New returns a new, empty CertificateRequest with a
// KeyRequest.
func New() *CertificateRequest {
	return &CertificateRequest{
		KeyRequest: NewKeyRequest(),
	}
}

// appendIf appends to a if s is not an empty string.
func appendIf(s string, a *[]string) {
	if s != "" {
		*a = append(*a, s)
	}
}

// OIDFromString creates an ASN1 ObjectIdentifier from its string representation
func OIDFromString(s string) (asn1.ObjectIdentifier, error) {
	var oid []int
	parts := strings.Split(s, ".")
	if len(parts) < 1 {
		return oid, fmt.Errorf("invalid OID string: %s", s)
	}
	for _, p := range parts {
		i, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("invalid OID part %s", p)
		}
		oid = append(oid, i)
	}
	return oid, nil
}

// Name returns the PKIX name for the request.
func (cr *CertificateRequest) Name() (pkix.Name, error) {
	var name pkix.Name
	name.CommonName = cr.CN

	for _, n := range cr.Names {
		appendIf(n.C, &name.Country)
		appendIf(n.ST, &name.Province)
		appendIf(n.L, &name.Locality)
		appendIf(n.O, &name.Organization)
		appendIf(n.OU, &name.OrganizationalUnit)
		for k, v := range n.OID {
			oid, err := OIDFromString(k)
			if err != nil {
				return name, err
			}
			name.ExtraNames = append(name.ExtraNames, pkix.AttributeTypeAndValue{Type: oid, Value: v})
		}
		if n.E != "" {
			name.ExtraNames = append(name.ExtraNames, pkix.AttributeTypeAndValue{Type: asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 1}, Value: n.E})
		}
	}
	name.SerialNumber = cr.SerialNumber
	return name, nil
}

// BasicConstraints CSR information RFC 5280, 4.2.1.9
type BasicConstraints struct {
	IsCA       bool `asn1:"optional"`
	MaxPathLen int  `asn1:"optional,default:-1"`
}

// ParseRequest takes a certificate request and generates a key and
// CSR from it. It does no validation -- caveat emptor. It will,
// however, fail if the key request is not valid (i.e., an unsupported
// curve or RSA key size). The lack of validation was specifically
// chosen to allow the end user to define a policy and validate the
// request appropriately before calling this function.
func ParseRequest(req *CertificateRequest) (csr, key []byte, err error) {
	log.Info("received CSR")
	if req.KeyRequest == nil {
		req.KeyRequest = NewKeyRequest()
	}

	log.Infof("generating key: %s-%d", req.KeyRequest.Algo(), req.KeyRequest.Size())
	priv, err := req.KeyRequest.Generate()
	if err != nil {
		err = cferr.Wrap(cferr.PrivateKeyError, cferr.GenerationFailed, err)
		return
	}

	switch priv := priv.(type) {
	case *rsa.PrivateKey:
		key = x509.MarshalPKCS1PrivateKey(priv)
		block := pem.Block{
			Type:  "RSA PRIVATE KEY",
			Bytes: key,
		}
		key = pem.EncodeToMemory(&block)
	case *ecdsa.PrivateKey:
		key, err = x509.MarshalECPrivateKey(priv)
		if err != nil {
			err = cferr.Wrap(cferr.PrivateKeyError, cferr.Unknown, err)
			return
		}
		block := pem.Block{
			Type:  "EC PRIVATE KEY",
			Bytes: key,
		}
		key = pem.EncodeToMemory(&block)
	case ed25519.PrivateKey:
		key, err = derhelpers.MarshalEd25519PrivateKey(priv)
		if err != nil {
			err = cferr.Wrap(cferr.PrivateKeyError, cferr.Unknown, err)
			return
		}
		block := pem.Block{
			Type:  "Ed25519 PRIVATE KEY",
			Bytes: key,
		}
		key = pem.EncodeToMemory(&block)
	case *mldsa.PrivateKey:
		key, err = x509.MarshalPKCS8PrivateKey(priv)
		if err != nil {
			err = cferr.Wrap(cferr.PrivateKeyError, cferr.Unknown, err)
			return
		}
		block := pem.Block{
			Type:  "PRIVATE KEY",
			Bytes: key,
		}
		key = pem.EncodeToMemory(&block)
	default:
		err = cferr.Wrap(cferr.PrivateKeyError, cferr.Unknown,
			fmt.Errorf("unsupported generated private key type %T", priv))
		return
	}

	csr, err = Generate(priv.(crypto.Signer), req)
	if err != nil {
		log.Errorf("failed to generate a CSR: %v", err)
		err = cferr.Wrap(cferr.CSRError, cferr.BadRequest, err)
	}
	return
}

// ExtractCertificateRequest extracts a CertificateRequest from
// x509.Certificate. It is aimed to used for generating a new certificate
// from an existing certificate. For a root certificate, the CA expiry
// length is calculated as the duration between cert.NotAfter and cert.NotBefore.
func ExtractCertificateRequest(cert *x509.Certificate) *CertificateRequest {
	req := New()
	req.CN = cert.Subject.CommonName
	req.Names = getNames(cert.Subject)
	req.Hosts = getHosts(cert)
	req.SerialNumber = cert.Subject.SerialNumber

	if cert.IsCA {
		req.CA = new(CAConfig)
		// CA expiry length is calculated based on the input cert
		// issue date and expiry date.
		req.CA.Expiry = cert.NotAfter.Sub(cert.NotBefore).String()
		req.CA.PathLength = cert.MaxPathLen
		req.CA.PathLenZero = cert.MaxPathLenZero
	}

	return req
}

func getHosts(cert *x509.Certificate) []string {
	var hosts []string
	for _, ip := range cert.IPAddresses {
		hosts = append(hosts, ip.String())
	}
	hosts = append(hosts, cert.DNSNames...)
	hosts = append(hosts, cert.EmailAddresses...)
	for _, uri := range cert.URIs {
		hosts = append(hosts, uri.String())
	}
	return hosts
}

// getNames returns an array of Names from the certificate
// It only cares about Country, Organization, OrganizationalUnit, Locality, Province
func getNames(sub pkix.Name) []Name {
	// anonymous func for finding the max of a list of integer
	max := func(v1 int, vn ...int) (max int) {
		max = v1
		for i := 0; i < len(vn); i++ {
			if vn[i] > max {
				max = vn[i]
			}
		}
		return max
	}

	nc := len(sub.Country)
	norg := len(sub.Organization)
	nou := len(sub.OrganizationalUnit)
	nl := len(sub.Locality)
	np := len(sub.Province)

	n := max(nc, norg, nou, nl, np)

	names := make([]Name, n)
	for i := range names {
		if i < nc {
			names[i].C = sub.Country[i]
		}
		if i < norg {
			names[i].O = sub.Organization[i]
		}
		if i < nou {
			names[i].OU = sub.OrganizationalUnit[i]
		}
		if i < nl {
			names[i].L = sub.Locality[i]
		}
		if i < np {
			names[i].ST = sub.Province[i]
		}
	}
	return names
}

// A Generator is responsible for validating certificate requests.
type Generator struct {
	Validator func(*CertificateRequest) error
}

// ProcessRequest validates and processes the incoming request. It is
// a wrapper around a validator and the ParseRequest function.
func (g *Generator) ProcessRequest(req *CertificateRequest) (csr, key []byte, err error) {

	log.Info("generate received request")
	err = g.Validator(req)
	if err != nil {
		log.Warningf("invalid request: %v", err)
		return nil, nil, err
	}

	csr, key, err = ParseRequest(req)
	if err != nil {
		return nil, nil, err
	}
	return
}

// IsNameEmpty returns true if the name has no identifying information in it.
func IsNameEmpty(n Name) bool {
	empty := func(s string) bool { return strings.TrimSpace(s) == "" }

	if empty(n.C) && empty(n.ST) && empty(n.L) && empty(n.O) && empty(n.OU) {
		return true
	}
	return false
}

// Regenerate uses the provided CSR as a template for signing a new
// CSR using priv.
func Regenerate(priv crypto.Signer, csr []byte) ([]byte, error) {
	req, extra, err := helpers.ParseCSR(csr)
	if err != nil {
		return nil, err
	} else if len(extra) > 0 {
		return nil, errors.New("csr: trailing data in certificate request")
	}

	return x509.CreateCertificateRequest(rand.Reader, req, priv)
}

// GenerateDER creates a new CSR(ASN1 DER encoded) from a CertificateRequest structure and
// an existing key. The KeyRequest field is ignored.
func GenerateDER(priv crypto.Signer, req *CertificateRequest) (csr []byte, err error) {
	sigAlgo := helpers.SignerAlgo(priv)
	if sigAlgo == x509.UnknownSignatureAlgorithm {
		return nil, cferr.New(cferr.PrivateKeyError, cferr.Unavailable)
	}

	subj, err := req.Name()
	if err != nil {
		return nil, err
	}

	var tpl = x509.CertificateRequest{
		Subject:            subj,
		SignatureAlgorithm: sigAlgo,
	}

	for i := range req.Hosts {
		if ip := net.ParseIP(req.Hosts[i]); ip != nil {
			tpl.IPAddresses = append(tpl.IPAddresses, ip)
		} else if email, err := mail.ParseAddress(req.Hosts[i]); err == nil && email != nil {
			tpl.EmailAddresses = append(tpl.EmailAddresses, email.Address)
		} else if uri, err := url.ParseRequestURI(req.Hosts[i]); err == nil && uri != nil {
			tpl.URIs = append(tpl.URIs, uri)
		} else {
			tpl.DNSNames = append(tpl.DNSNames, req.Hosts[i])
		}
	}

	tpl.ExtraExtensions = []pkix.Extension{}

	if req.CA != nil {
		err = appendCAInfoToCSR(req.CA, &tpl)
		if err != nil {
			err = cferr.Wrap(cferr.CSRError, cferr.GenerationFailed, err)
			return
		}
	}

	if req.DelegationEnabled {
		tpl.ExtraExtensions = append(tpl.Extensions, helpers.DelegationExtension)
	}

	if req.Extensions != nil {
		err = appendExtensionsToCSR(req.Extensions, &tpl)
		if err != nil {
			err = cferr.Wrap(cferr.CSRError, cferr.GenerationFailed, err)
			return
		}
	}

	csr, err = x509.CreateCertificateRequest(rand.Reader, &tpl, priv)
	if err != nil {
		log.Errorf("failed to generate a CSR: %v", err)
		err = cferr.Wrap(cferr.CSRError, cferr.BadRequest, err)
		return
	}
	return
}

// Generate creates a new CSR(PEM encoded) from a CertificateRequest structure and
// an existing key. The KeyRequest field is ignored.
func Generate(priv crypto.Signer, req *CertificateRequest) (csr []byte, err error) {

	csr, err = GenerateDER(priv, req)
	if err != nil {
		return
	}
	block := pem.Block{
		Type:  "CERTIFICATE REQUEST",
		Bytes: csr,
	}

	log.Info("encoded CSR")
	csr = pem.EncodeToMemory(&block)
	return
}

// appendCAInfoToCSR appends CAConfig BasicConstraint extension to a CSR
func appendCAInfoToCSR(reqConf *CAConfig, csr *x509.CertificateRequest) error {
	pathlen := reqConf.PathLength
	if pathlen == 0 && !reqConf.PathLenZero {
		pathlen = -1
	}
	val, err := asn1.Marshal(BasicConstraints{true, pathlen})

	if err != nil {
		return err
	}

	csr.ExtraExtensions = append(csr.ExtraExtensions, pkix.Extension{
		Id:       asn1.ObjectIdentifier{2, 5, 29, 19},
		Value:    val,
		Critical: true,
	})

	return nil
}

// appendCAInfoToCSR appends user-defined extension to a CSR
func appendExtensionsToCSR(extensions []pkix.Extension, csr *x509.CertificateRequest) error {
	csr.ExtraExtensions = append(csr.ExtraExtensions, extensions...)
	return nil
}
