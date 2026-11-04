package signing

import (
	"bytes"
	"context"
	"os/exec"
	"slices"

	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
)

// Signer signs the exact payload bytes and verifies the resulting bundle.
type Signer interface {
	Sign(context.Context, []byte) ([]byte, error)
}

// NewSigner returns a command-based signer using verifier to check its output.
// The command is an argv array configured by the daemon administrator.
func NewSigner(command []string, verifier Verifier) Signer {
	return &commandSigner{command: slices.Clone(command), verifier: verifier}
}

type commandSigner struct {
	command  []string
	verifier Verifier
}

func (s *commandSigner) Sign(ctx context.Context, payload []byte) ([]byte, error) {
	if len(s.command) == 0 {
		return nil, errors.New("no signing command configured")
	}
	if s.verifier == nil {
		return nil, errors.New("no verifier available for signed cache")
	}
	cmd := exec.CommandContext(ctx, s.command[0], s.command[1:]...) //nolint:gosec // defined in toml config
	cmd.Stdin = bytes.NewReader(payload)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, errors.Wrapf(err, "signing command failed: %s", stderr.String())
	}
	if err := s.verifier.Verify(ctx, digest.FromBytes(payload), out.Bytes()); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
