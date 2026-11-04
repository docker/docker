package signing

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/moby/buildkit/util/bklog"
	policy "github.com/moby/policy-helpers"
	"github.com/moby/policy-helpers/types"
	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
	"github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
)

// Verifier authenticates a bundle against the digest and the configured policy.
type Verifier interface {
	Verify(context.Context, digest.Digest, []byte) error
}

// VerifierProvider lazily supplies the daemon's shared Sigstore verifier.
type VerifierProvider func() (*policy.Verifier, error)

// NewVerifier returns a verifier applying the given policy to cache signatures.
func NewVerifier(rules VerifyPolicy, provider VerifierProvider) Verifier {
	return &verifier{rules: rules, provider: provider}
}

type verifier struct {
	rules    VerifyPolicy
	provider VerifierProvider
}

func (v *verifier) Verify(ctx context.Context, dgst digest.Digest, bundle []byte) error {
	if v.provider == nil {
		return errors.New("no verifier available for signed cache")
	}
	artifactVerifier, err := v.provider()
	if err != nil {
		return err
	}
	if artifactVerifier == nil {
		return errors.New("no verifier available for signed cache")
	}
	opts := []policy.ArtifactVerifyOpt{policy.WithSLSANotRequired()}
	if v.rules.TlogThreshold != nil {
		opts = append(opts, policy.WithTlogThreshold(*v.rules.TlogThreshold))
	}
	sig, err := artifactVerifier.VerifyArtifact(ctx, dgst, bundle, opts...)
	if err != nil {
		return err
	}
	return verifyPolicy(ctx, sig, v.rules)
}

// verifyPolicy checks the authenticated certificate and timestamps.
func verifyPolicy(ctx context.Context, sig *types.SignatureInfo, policyRules VerifyPolicy) error {
	if sig.Signer == nil {
		return errors.New("signature verification failed: no signer found")
	}
	numTimestamps := len(sig.Timestamps)
	if policyRules.TimestampThreshold > numTimestamps {
		return errors.Errorf("signature verification failed: not enough timestamp authorities: have %d, need %d", numTimestamps, policyRules.TimestampThreshold)
	}

	certRules, err := certToStringMap(&policyRules.Summary)
	if err != nil {
		return err
	}
	certFields, err := certToStringMap(sig.Signer)
	if err != nil {
		return err
	}
	bklog.G(ctx).Debugf("signature verification: %+v", sig)
	bklog.G(ctx).Debugf("signer: %+v", sig.Signer)
	for k, v := range certRules {
		if v == "" {
			continue
		}
		if !simplePatternMatch(v, certFields[k]) {
			return errors.Errorf("signature verification failed: certificate field %q does not match policy (%q != %q)", k, certFields[k], v)
		}
		bklog.G(ctx).Debugf("certificate field %q matches policy (%q)", k, certFields[k])
	}
	return nil
}

func certToStringMap(cert *certificate.Summary) (map[string]string, error) {
	dt, err := json.Marshal(cert)
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	if err := json.Unmarshal(dt, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func simplePatternMatch(pat, s string) bool {
	if pat == "*" {
		return true
	}
	if strings.HasPrefix(pat, "*") && strings.HasSuffix(pat, "*") {
		return strings.Contains(s, pat[1:len(pat)-1])
	}
	if strings.HasPrefix(pat, "*") {
		return strings.HasSuffix(s, pat[1:])
	}
	if strings.HasSuffix(pat, "*") {
		return strings.HasPrefix(s, pat[:len(pat)-1])
	}
	return s == pat
}
