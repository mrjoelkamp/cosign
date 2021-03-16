/*
Copyright The Rekor Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cosign

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io/ioutil"
	"runtime"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"
)

type SignedPayload struct {
	Base64Signature string
	Payload         []byte
	Cert            *x509.Certificate
	Chain           []*x509.Certificate
}

// TODO: marshal the cert correctly.
// func (sp *SignedPayload) MarshalJSON() ([]byte, error) {
// 	x509.Certificate.
// 	pem.EncodeToMemory(&pem.Block{
// 		Type: "CERTIFICATE",
// 		Bytes:
// 	})
// }

func Munge(desc v1.Descriptor) string {
	// sha256:... -> sha256-...
	munged := strings.ReplaceAll(desc.Digest.String(), ":", "-")
	munged += ".cosign"
	return munged
}

func FetchSignatures(ctx context.Context, ref name.Reference) ([]SignedPayload, *v1.Descriptor, error) {
	var sigRef name.Reference
	targetDesc, err := remote.Get(ref, remote.WithAuthFromKeychain(authn.DefaultKeychain))
	if err != nil {
		return nil, nil, err
	}
	sigRef = ref.Context().Tag(Munge(targetDesc.Descriptor))

	sigImg, err := remote.Image(sigRef, remote.WithAuthFromKeychain(authn.DefaultKeychain))
	if err != nil {
		return nil, nil, err
	}

	m, err := sigImg.Manifest()
	if err != nil {
		return nil, nil, err
	}

	g, ctx := errgroup.WithContext(ctx)
	signatures := make([]SignedPayload, len(m.Layers))
	sem := semaphore.NewWeighted(int64(runtime.NumCPU()))
	for i, desc := range m.Layers {
		i, desc := i, desc
		if err := sem.Acquire(ctx, 1); err != nil {
			return nil, nil, err
		}
		g.Go(func() error {
			defer sem.Release(1)
			base64sig, ok := desc.Annotations[sigkey]
			if !ok {
				return nil
			}
			l, err := sigImg.LayerByDigest(desc.Digest)
			if err != nil {
				return err
			}

			// Compressed is a misnomer here, we just want the raw bytes from the registry.
			r, err := l.Compressed()
			if err != nil {
				return err

			}

			payload, err := ioutil.ReadAll(r)
			if err != nil {
				return err
			}
			sp := SignedPayload{
				Payload:         payload,
				Base64Signature: base64sig,
			}
			// We may have a certificate and chain
			certPem := desc.Annotations[certkey]
			if certPem != "" {
				certs, err := LoadCerts(certPem)
				if err != nil {
					return err
				}
				if len(certs) != 1 {
					return fmt.Errorf("expected 1 certificate, found %d", len(certs))
				}
				sp.Cert = certs[0]
			}
			chainPem := desc.Annotations[chainkey]
			if chainPem != "" {
				certs, err := LoadCerts(chainPem)
				if err != nil {
					return err
				}
				sp.Chain = certs
			}
			signatures[i] = SignedPayload{
				Payload:         payload,
				Base64Signature: base64sig,
			}
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, nil, err
	}
	return signatures, &targetDesc.Descriptor, nil
}

func LoadCerts(pemStr string) ([]*x509.Certificate, error) {
	blocks := []*pem.Block{}
	pemBytes := []byte(pemStr)
	for {
		block, rest := pem.Decode(pemBytes)
		if block == nil {
			break
		}
		if block.Type == "CERTIFICATE" {
			blocks = append(blocks, block)
		}
		pemBytes = rest
	}

	certs := []*x509.Certificate{}
	for _, block := range blocks {
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		certs = append(certs, cert)
	}
	return certs, nil
}
