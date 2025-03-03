//
// Copyright 2021 The Sigstore Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sign

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/sigstore/cosign/v2/cmd/cosign/cli/fulcio"
	"github.com/sigstore/cosign/v2/cmd/cosign/cli/fulcio/fulcioverifier"
	"github.com/sigstore/cosign/v2/cmd/cosign/cli/options"
	"github.com/sigstore/cosign/v2/cmd/cosign/cli/rekor"
	"github.com/sigstore/cosign/v2/cmd/cosign/cli/sign/privacy"
	icos "github.com/sigstore/cosign/v2/internal/pkg/cosign"
	ifulcio "github.com/sigstore/cosign/v2/internal/pkg/cosign/fulcio"
	ipayload "github.com/sigstore/cosign/v2/internal/pkg/cosign/payload"
	irekor "github.com/sigstore/cosign/v2/internal/pkg/cosign/rekor"
	"github.com/sigstore/cosign/v2/internal/pkg/cosign/tsa"
	"github.com/sigstore/cosign/v2/internal/ui"
	"github.com/sigstore/cosign/v2/pkg/cosign"
	"github.com/sigstore/cosign/v2/pkg/cosign/pivkey"
	"github.com/sigstore/cosign/v2/pkg/cosign/pkcs11key"
	cremote "github.com/sigstore/cosign/v2/pkg/cosign/remote"
	"github.com/sigstore/cosign/v2/pkg/oci"
	"github.com/sigstore/cosign/v2/pkg/oci/mutate"
	ociremote "github.com/sigstore/cosign/v2/pkg/oci/remote"
	"github.com/sigstore/cosign/v2/pkg/oci/walk"
	sigs "github.com/sigstore/cosign/v2/pkg/signature"
	"github.com/sigstore/sigstore/pkg/cryptoutils"
	"github.com/sigstore/sigstore/pkg/signature"
	signatureoptions "github.com/sigstore/sigstore/pkg/signature/options"
	sigPayload "github.com/sigstore/sigstore/pkg/signature/payload"
	tsaclient "github.com/sigstore/timestamp-authority/pkg/client"

	// Loads OIDC providers
	_ "github.com/sigstore/cosign/v2/pkg/providers/all"
)

const TagReferenceMessage string = `Image reference %s uses a tag, not a digest, to identify the image to sign.

    This can lead you to sign a different image than the intended one. Please use a
    digest (example.com/ubuntu@sha256:abc123...) rather than tag
    (example.com/ubuntu:latest) for the input to cosign. The ability to refer to
    images by tag will be removed in a future release.
`

func ShouldUploadToTlog(ctx context.Context, ko options.KeyOpts, ref name.Reference, tlogUpload bool) (bool, error) {
	upload := shouldUploadToTlog(ctx, ko, ref, tlogUpload)
	var statementErr error
	if upload {
		privacy.StatementOnce.Do(func() {
			ui.Info(ctx, privacy.Statement)
			ui.Info(ctx, privacy.StatementConfirmation)
			if !ko.SkipConfirmation {
				if err := ui.ConfirmContinue(ctx); err != nil {
					statementErr = err
				}
			}
		})
	}
	return upload, statementErr
}

func shouldUploadToTlog(ctx context.Context, ko options.KeyOpts, ref name.Reference, tlogUpload bool) bool {
	// return false if not uploading to the tlog has been requested
	if !tlogUpload {
		return false
	}

	if ko.SkipConfirmation {
		return true
	}

	// We don't need to validate the ref, just return true
	if ref == nil {
		return true
	}

	// Check if the image is public (no auth in Get)
	if _, err := remote.Get(ref, remote.WithContext(ctx)); err != nil {
		ui.Warn(ctx, "%q appears to be a private repository, please confirm uploading to the transparency log at %q", ref.Context().String(), ko.RekorURL)
		var errPromptDeclined *ui.ErrPromptDeclined
		if err := ui.ConfirmContinue(ctx); errors.As(err, &errPromptDeclined) {
			ui.Info(ctx, "not uploading to transparency log")
		} else if err != nil {
			ui.Warn(ctx, "skipping transparency log upload (use --yes to skip confirmation): %v\n", err)
			return false
		}
	}
	return true
}

func GetAttachedImageRef(ref name.Reference, attachment string, opts ...ociremote.Option) (name.Reference, error) {
	if attachment == "" {
		return ref, nil
	}
	if attachment == "sbom" {
		return ociremote.SBOMTag(ref, opts...)
	}
	return nil, fmt.Errorf("unknown attachment type %s", attachment)
}

// ParseOCIReference parses a string reference to an OCI image into a reference, warning if the reference did not include a digest.
func ParseOCIReference(ctx context.Context, refStr string, opts ...name.Option) (name.Reference, error) {
	ref, err := name.ParseReference(refStr, opts...)
	if err != nil {
		return nil, fmt.Errorf("parsing reference: %w", err)
	}
	if _, ok := ref.(name.Digest); !ok {
		msg := fmt.Sprintf(TagReferenceMessage, refStr)
		ui.Warn(ctx, msg)
	}
	return ref, nil
}

// nolint
func SignCmd(ro *options.RootOptions, ko options.KeyOpts, signOpts options.SignOptions, imgs []string) error {
	if options.NOf(ko.KeyRef, ko.Sk) > 1 {
		return &options.KeyParseError{}
	}

	ctx, cancel := context.WithTimeout(context.Background(), ro.Timeout)
	defer cancel()

	sv, err := SignerFromKeyOpts(ctx, signOpts.Cert, signOpts.CertChain, ko)
	if err != nil {
		return fmt.Errorf("getting signer: %w", err)
	}
	defer sv.Close()
	dd := cremote.NewDupeDetector(sv)

	var staticPayload []byte
	if signOpts.PayloadPath != "" {
		fmt.Fprintln(os.Stderr, "Using payload from:", signOpts.PayloadPath)
		staticPayload, err = os.ReadFile(filepath.Clean(signOpts.PayloadPath))
		if err != nil {
			return fmt.Errorf("payload from file: %w", err)
		}
	}

	// Set up an ErrDone consideration to return along "success" paths
	var ErrDone error
	if !signOpts.Recursive {
		ErrDone = mutate.ErrSkipChildren
	}
	regOpts := signOpts.Registry
	opts, err := regOpts.ClientOpts(ctx)
	if err != nil {
		return fmt.Errorf("constructing client options: %w", err)
	}
	am, err := signOpts.AnnotationsMap()
	if err != nil {
		return fmt.Errorf("getting annotations: %w", err)
	}
	annotations := am.Annotations
	for _, inputImg := range imgs {
		ref, err := ParseOCIReference(ctx, inputImg, regOpts.NameOptions()...)
		if err != nil {
			return err
		}
		ref, err = GetAttachedImageRef(ref, signOpts.Attachment, opts...)
		if err != nil {
			return fmt.Errorf("unable to resolve attachment %s for image %s", signOpts.Attachment, inputImg)
		}

		if digest, ok := ref.(name.Digest); ok && !signOpts.Recursive {
			se, err := ociremote.SignedEntity(ref, opts...)
			if err != nil {
				return fmt.Errorf("accessing image: %w", err)
			}
			err = signDigest(ctx, digest, staticPayload, ko, regOpts, annotations, signOpts.Upload, signOpts.OutputSignature, signOpts.OutputCertificate, signOpts.Recursive, signOpts.TlogUpload, dd, sv, se)
			if err != nil {
				return fmt.Errorf("signing digest: %w", err)
			}
			continue
		}

		se, err := ociremote.SignedEntity(ref, opts...)
		if err != nil {
			return fmt.Errorf("accessing entity: %w", err)
		}

		if err := walk.SignedEntity(ctx, se, func(ctx context.Context, se oci.SignedEntity) error {
			// Get the digest for this entity in our walk.
			d, err := se.(interface{ Digest() (v1.Hash, error) }).Digest()
			if err != nil {
				return fmt.Errorf("computing digest: %w", err)
			}
			digest := ref.Context().Digest(d.String())
			err = signDigest(ctx, digest, staticPayload, ko, regOpts, annotations, signOpts.Upload, signOpts.OutputSignature, signOpts.OutputCertificate, signOpts.Recursive, signOpts.TlogUpload, dd, sv, se)
			if err != nil {
				return fmt.Errorf("signing digest: %w", err)
			}
			return ErrDone
		}); err != nil {
			return fmt.Errorf("recursively signing: %w", err)
		}
	}

	return nil
}

func signDigest(ctx context.Context, digest name.Digest, payload []byte, ko options.KeyOpts,
	regOpts options.RegistryOptions, annotations map[string]interface{}, upload bool, outputSignature, outputCertificate string, recursive bool, tlogUpload bool,
	dd mutate.DupeDetector, sv *SignerVerifier, se oci.SignedEntity) error {
	var err error
	// The payload can be passed to skip generation.
	if len(payload) == 0 {
		payload, err = (&sigPayload.Cosign{
			Image:       digest,
			Annotations: annotations,
		}).MarshalJSON()
		if err != nil {
			return fmt.Errorf("payload: %w", err)
		}
	}

	var s icos.Signer
	s = ipayload.NewSigner(sv)
	if sv.Cert != nil {
		s = ifulcio.NewSigner(s, sv.Cert, sv.Chain)
	}

	if ko.TSAServerURL != "" {
		clientTSA, err := tsaclient.GetTimestampClient(ko.TSAServerURL)
		if err != nil {
			return fmt.Errorf("failed to create TSA client: %w", err)
		}

		s = tsa.NewSigner(s, clientTSA)
	}
	shouldUpload, err := ShouldUploadToTlog(ctx, ko, digest, tlogUpload)
	if err != nil {
		return fmt.Errorf("should upload to tlog: %w", err)
	}
	if shouldUpload {
		rClient, err := rekor.NewClient(ko.RekorURL)
		if err != nil {
			return err
		}
		s = irekor.NewSigner(s, rClient)
	}

	ociSig, _, err := s.Sign(ctx, bytes.NewReader(payload))
	if err != nil {
		return err
	}

	b64sig, err := ociSig.Base64Signature()
	if err != nil {
		return err
	}

	if outputSignature != "" {
		// Add digest to suffix to differentiate each image during recursive signing
		if recursive {
			outputSignature = fmt.Sprintf("%s-%s", outputSignature, strings.Replace(digest.DigestStr(), ":", "-", 1))
		}
		if err := os.WriteFile(outputSignature, []byte(b64sig), 0600); err != nil {
			return fmt.Errorf("create signature file: %w", err)
		}
	}

	if outputCertificate != "" {
		rekorBytes, err := sv.Bytes(ctx)
		if err != nil {
			return fmt.Errorf("create certificate file: %w", err)
		}

		if err := os.WriteFile(outputCertificate, rekorBytes, 0600); err != nil {
			return fmt.Errorf("create certificate file: %w", err)
		}
		// TODO: maybe accept a --b64 flag as well?
		fmt.Printf("Certificate wrote in the file %s\n", outputCertificate)
	}

	if !upload {
		return nil
	}

	// Attach the signature to the entity.
	newSE, err := mutate.AttachSignatureToEntity(se, ociSig, mutate.WithDupeDetector(dd))
	if err != nil {
		return err
	}

	// Publish the signatures associated with this entity
	walkOpts, err := regOpts.ClientOpts(ctx)
	if err != nil {
		return fmt.Errorf("constructing client options: %w", err)
	}

	// Check if we are overriding the signatures repository location
	repo, _ := ociremote.GetEnvTargetRepository()
	if repo.RepositoryStr() == "" {
		fmt.Fprintln(os.Stderr, "Pushing signature to:", digest.Repository)
	} else {
		fmt.Fprintln(os.Stderr, "Pushing signature to:", repo.RepositoryStr())
	}

	// Publish the signatures associated with this entity
	if err := ociremote.WriteSignatures(digest.Repository, newSE, walkOpts...); err != nil {
		return err
	}

	return nil
}

func signerFromSecurityKey(keySlot string) (*SignerVerifier, error) {
	sk, err := pivkey.GetKeyWithSlot(keySlot)
	if err != nil {
		return nil, err
	}
	sv, err := sk.SignerVerifier()
	if err != nil {
		sk.Close()
		return nil, err
	}

	// Handle the -cert flag.
	// With PIV, we assume the certificate is in the same slot on the PIV
	// token as the private key. If it's not there, show a warning to the
	// user.
	certFromPIV, err := sk.Certificate()
	var pemBytes []byte
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: no x509 certificate retrieved from the PIV token")
	} else {
		pemBytes, err = cryptoutils.MarshalCertificateToPEM(certFromPIV)
		if err != nil {
			sk.Close()
			return nil, err
		}
	}

	return &SignerVerifier{
		Cert:           pemBytes,
		SignerVerifier: sv,
		close:          sk.Close,
	}, nil
}

func signerFromKeyRef(ctx context.Context, certPath, certChainPath, keyRef string, passFunc cosign.PassFunc) (*SignerVerifier, error) {
	k, err := sigs.SignerVerifierFromKeyRef(ctx, keyRef, passFunc)
	if err != nil {
		return nil, fmt.Errorf("reading key: %w", err)
	}
	certSigner := &SignerVerifier{
		SignerVerifier: k,
	}

	var leafCert *x509.Certificate

	// Attempt to extract certificate from PKCS11 token
	// With PKCS11, we assume the certificate is in the same slot on the PKCS11
	// token as the private key. If it's not there, show a warning to the
	// user.
	if pkcs11Key, ok := k.(*pkcs11key.Key); ok {
		certFromPKCS11, _ := pkcs11Key.Certificate()
		certSigner.close = pkcs11Key.Close

		if certFromPKCS11 == nil {
			fmt.Fprintln(os.Stderr, "warning: no x509 certificate retrieved from the PKCS11 token")
		} else {
			pemBytes, err := cryptoutils.MarshalCertificateToPEM(certFromPKCS11)
			if err != nil {
				pkcs11Key.Close()
				return nil, err
			}
			// Check that the provided public key and certificate key match
			pubKey, err := k.PublicKey()
			if err != nil {
				pkcs11Key.Close()
				return nil, err
			}
			if cryptoutils.EqualKeys(pubKey, certFromPKCS11.PublicKey) != nil {
				pkcs11Key.Close()
				return nil, errors.New("pkcs11 key and certificate do not match")
			}
			leafCert = certFromPKCS11
			certSigner.Cert = pemBytes
		}
	}

	// Handle --cert flag
	if certPath != "" {
		// Allow both DER and PEM encoding
		certBytes, err := os.ReadFile(certPath)
		if err != nil {
			return nil, fmt.Errorf("read certificate: %w", err)
		}
		// Handle PEM
		if bytes.HasPrefix(certBytes, []byte("-----")) {
			decoded, _ := pem.Decode(certBytes)
			if decoded.Type != "CERTIFICATE" {
				return nil, fmt.Errorf("supplied PEM file is not a certificate: %s", certPath)
			}
			certBytes = decoded.Bytes
		}
		parsedCert, err := x509.ParseCertificate(certBytes)
		if err != nil {
			return nil, fmt.Errorf("parse x509 certificate: %w", err)
		}
		pk, err := k.PublicKey()
		if err != nil {
			return nil, fmt.Errorf("get public key: %w", err)
		}
		if cryptoutils.EqualKeys(pk, parsedCert.PublicKey) != nil {
			return nil, errors.New("public key in certificate does not match the provided public key")
		}
		pemBytes, err := cryptoutils.MarshalCertificateToPEM(parsedCert)
		if err != nil {
			return nil, fmt.Errorf("marshaling certificate to PEM: %w", err)
		}
		if certSigner.Cert != nil {
			fmt.Fprintln(os.Stderr, "warning: overriding x509 certificate retrieved from the PKCS11 token")
		}
		leafCert = parsedCert
		certSigner.Cert = pemBytes
	}

	if certChainPath == "" {
		return certSigner, nil
	} else if certSigner.Cert == nil {
		return nil, errors.New("no leaf certificate found or provided while specifying chain")
	}

	// Handle --cert-chain flag
	// Accept only PEM encoded certificate chain
	certChainBytes, err := os.ReadFile(certChainPath)
	if err != nil {
		return nil, fmt.Errorf("reading certificate chain from path: %w", err)
	}
	certChain, err := cryptoutils.LoadCertificatesFromPEM(bytes.NewReader(certChainBytes))
	if err != nil {
		return nil, fmt.Errorf("loading certificate chain: %w", err)
	}
	if len(certChain) == 0 {
		return nil, errors.New("no certificates in certificate chain")
	}
	// Verify certificate chain is valid
	rootPool := x509.NewCertPool()
	rootPool.AddCert(certChain[len(certChain)-1])
	subPool := x509.NewCertPool()
	for _, c := range certChain[:len(certChain)-1] {
		subPool.AddCert(c)
	}
	if _, err := cosign.TrustedCert(leafCert, rootPool, subPool); err != nil {
		return nil, fmt.Errorf("unable to validate certificate chain: %w", err)
	}
	// Verify SCT if present in the leaf certificate.
	contains, err := cosign.ContainsSCT(leafCert.Raw)
	if err != nil {
		return nil, err
	}
	if contains {
		pubKeys, err := cosign.GetCTLogPubs(ctx)
		if err != nil {
			return nil, fmt.Errorf("getting CTLog public keys: %w", err)
		}
		var chain []*x509.Certificate
		chain = append(chain, leafCert)
		chain = append(chain, certChain...)
		if err := cosign.VerifyEmbeddedSCT(context.Background(), chain, pubKeys); err != nil {
			return nil, err
		}
	}
	certSigner.Chain = certChainBytes

	return certSigner, nil
}

func keylessSigner(ctx context.Context, ko options.KeyOpts) (*SignerVerifier, error) {
	var (
		k   *fulcio.Signer
		err error
	)

	if ko.InsecureSkipFulcioVerify {
		if k, err = fulcio.NewSigner(ctx, ko); err != nil {
			return nil, fmt.Errorf("getting key from Fulcio: %w", err)
		}
	} else {
		if k, err = fulcioverifier.NewSigner(ctx, ko); err != nil {
			return nil, fmt.Errorf("getting key from Fulcio: %w", err)
		}
	}

	return &SignerVerifier{
		Cert:           k.Cert,
		Chain:          k.Chain,
		SignerVerifier: k,
	}, nil
}

func SignerFromKeyOpts(ctx context.Context, certPath string, certChainPath string, ko options.KeyOpts) (*SignerVerifier, error) {
	if ko.Sk {
		return signerFromSecurityKey(ko.Slot)
	}

	if ko.KeyRef != "" {
		return signerFromKeyRef(ctx, certPath, certChainPath, ko.KeyRef, ko.PassFunc)
	}

	// Default Keyless!
	fmt.Fprintln(os.Stderr, "Generating ephemeral keys...")
	return keylessSigner(ctx, ko)
}

type SignerVerifier struct {
	Cert  []byte
	Chain []byte
	signature.SignerVerifier
	close func()
}

func (c *SignerVerifier) Close() {
	if c.close != nil {
		c.close()
	}
}

func (c *SignerVerifier) Bytes(ctx context.Context) ([]byte, error) {
	if c.Cert != nil {
		fmt.Fprintf(os.Stderr, "using ephemeral certificate:\n%s\n", string(c.Cert))
		return c.Cert, nil
	}

	pemBytes, err := sigs.PublicKeyPem(c, signatureoptions.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	return pemBytes, nil
}
