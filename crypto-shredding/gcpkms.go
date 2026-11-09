package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

// CloudKMS keeps one CryptoKey per tenant in a key ring and one key version
// per tenant per month. Destroying a version shreds that month for that
// tenant, in the database and in every backup of it.
type CloudKMS struct {
	client  *kms.KeyManagementClient
	keyRing string // projects/<p>/locations/<l>/keyRings/<r>
	// How long a destroyed version can still be restored. Cloud KMS allows
	// 24 hours to 120 days; the default is 30 days.
	undoWindow time.Duration
}

func OpenCloudKMS(ctx context.Context, keyRing string) (*CloudKMS, error) {
	c, err := kms.NewKeyManagementClient(ctx)
	if err != nil {
		return nil, err
	}
	return &CloudKMS{client: c, keyRing: keyRing, undoWindow: 30 * 24 * time.Hour}, nil
}

func (k *CloudKMS) NewVersion(ctx context.Context, tenant string) (string, error) {
	key := k.keyRing + "/cryptoKeys/" + tenant
	v, err := k.client.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{
		Parent: key,
	})
	if status.Code(err) == codes.NotFound {
		// First write for this tenant. In production, onboarding does this.
		if err := k.createKey(ctx, tenant); err != nil {
			return "", err
		}
		return k.NewVersion(ctx, tenant)
	}
	if err != nil {
		return "", err
	}
	return v.Name, nil
}

func (k *CloudKMS) createKey(ctx context.Context, tenant string) error {
	_, err := k.client.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{
		Parent:      k.keyRing,
		CryptoKeyId: tenant,
		CryptoKey: &kmspb.CryptoKey{
			Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT,
			VersionTemplate: &kmspb.CryptoKeyVersionTemplate{
				Algorithm:       kmspb.CryptoKeyVersion_GOOGLE_SYMMETRIC_ENCRYPTION,
				ProtectionLevel: kmspb.ProtectionLevel_SOFTWARE,
			},
			DestroyScheduledDuration: durationpb.New(k.undoWindow),
		},
		// Versions are created one per month by NewVersion, not here.
		SkipInitialVersionCreation: true,
	})
	if status.Code(err) == codes.AlreadyExists {
		return nil
	}
	return err
}

// Wrap encrypts with a named version rather than the key's primary, so the
// month a data key belongs to never depends on when rotation happened.
func (k *CloudKMS) Wrap(ctx context.Context, version string, dek, aad []byte) ([]byte, error) {
	resp, err := k.client.Encrypt(ctx, &kmspb.EncryptRequest{
		Name:                        version,
		Plaintext:                   dek,
		AdditionalAuthenticatedData: aad,
	})
	if err != nil {
		return nil, err
	}
	return resp.Ciphertext, nil
}

func (k *CloudKMS) Unwrap(ctx context.Context, version string, wrapped, aad []byte) ([]byte, error) {
	// Decrypt takes the key, not the version: the version is in the ciphertext.
	key, _, _ := strings.Cut(version, "/cryptoKeyVersions/")
	resp, err := k.client.Decrypt(ctx, &kmspb.DecryptRequest{
		Name:                        key,
		Ciphertext:                  wrapped,
		AdditionalAuthenticatedData: aad,
	})
	if status.Code(err) == codes.FailedPrecondition {
		// The version is scheduled for destruction, destroyed or disabled.
		return nil, fmt.Errorf("%s: %w", version, errors.Join(ErrDestroyed, err))
	}
	if err != nil {
		return nil, err
	}
	return resp.Plaintext, nil
}

// Destroy schedules the version for destruction. It stops working at once,
// can be restored during the undo window, and is gone for good after it.
func (k *CloudKMS) Destroy(ctx context.Context, version string) error {
	_, err := k.client.DestroyCryptoKeyVersion(ctx, &kmspb.DestroyCryptoKeyVersionRequest{
		Name: version,
	})
	return err
}
