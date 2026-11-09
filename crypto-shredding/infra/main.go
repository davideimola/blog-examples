// The GCP side of crypto-shredding: a key ring for per-tenant keys, two
// identities that split who encrypts from who destroys, KMS data access logs,
// and a Cloud SQL Postgres whose backups we no longer need to edit.
//
// The per-tenant keys are not declared here: tenants come and go at runtime,
// so the application creates a key on onboarding and a version per month.
// What the stack fixes are the rules those keys live under.
package main

import (
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/kms"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/projects"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/serviceaccount"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/sql"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

func main() {
	pulumi.Run(func(ctx *pulumi.Context) error {
		project := config.Require(ctx, "gcp:project")
		region := config.Get(ctx, "gcp:region")
		if region == "" {
			region = "europe-west8"
		}

		// Key rings cannot be deleted in GCP, so pick the name once.
		ring, err := kms.NewKeyRing(ctx, "agent-sessions", &kms.KeyRingArgs{
			Name:     pulumi.String("agent-sessions"),
			Location: pulumi.String(region),
		})
		if err != nil {
			return err
		}

		// The application writes and reads sessions. It can create a tenant's
		// key and a new version each month, and use them, but never destroy
		// one: a bug or a stolen token in the app cannot shred anything.
		app, err := serviceaccount.NewAccount(ctx, "sessions-app", &serviceaccount.AccountArgs{
			AccountId:   pulumi.String("sessions-app"),
			DisplayName: pulumi.String("Agent sessions: application"),
		})
		if err != nil {
			return err
		}
		keyUser, err := projects.NewIAMCustomRole(ctx, "session-key-user", &projects.IAMCustomRoleArgs{
			RoleId: pulumi.String("sessionKeyUser"),
			Title:  pulumi.String("Session key user"),
			Permissions: pulumi.ToStringArray([]string{
				"cloudkms.cryptoKeys.create",
				"cloudkms.cryptoKeys.get",
				"cloudkms.cryptoKeyVersions.create",
				"cloudkms.cryptoKeyVersions.useToEncrypt",
				"cloudkms.cryptoKeyVersions.useToDecrypt",
			}),
		})
		if err != nil {
			return err
		}

		// The retention job is the only identity that can destroy a version.
		// Minimum retention lives in its code: it refuses months inside the
		// window, and Cloud KMS keeps every destroyed version restorable for
		// the key's undo window on top of that.
		retention, err := serviceaccount.NewAccount(ctx, "sessions-retention", &serviceaccount.AccountArgs{
			AccountId:   pulumi.String("sessions-retention"),
			DisplayName: pulumi.String("Agent sessions: retention job"),
		})
		if err != nil {
			return err
		}
		shredder, err := projects.NewIAMCustomRole(ctx, "session-key-shredder", &projects.IAMCustomRoleArgs{
			RoleId: pulumi.String("sessionKeyShredder"),
			Title:  pulumi.String("Session key shredder"),
			Permissions: pulumi.ToStringArray([]string{
				"cloudkms.cryptoKeys.list",
				"cloudkms.cryptoKeyVersions.list",
				"cloudkms.cryptoKeyVersions.get",
				"cloudkms.cryptoKeyVersions.destroy",
			}),
		})
		if err != nil {
			return err
		}

		// Bound on the key ring, not the project: neither identity can touch
		// any other key.
		for name, b := range map[string]struct {
			role   *projects.IAMCustomRole
			member *serviceaccount.Account
		}{
			"app-key-user":       {keyUser, app},
			"retention-shredder": {shredder, retention},
		} {
			_, err := kms.NewKeyRingIAMMember(ctx, name, &kms.KeyRingIAMMemberArgs{
				KeyRingId: ring.ID(),
				Role:      b.role.Name,
				Member:    pulumi.Sprintf("serviceAccount:%s", b.member.Email),
			})
			if err != nil {
				return err
			}
		}

		// Every encrypt, decrypt and destroy leaves a log entry. If someone
		// steals the app's identity, rotation will not stop them: this is how
		// you see them.
		_, err = projects.NewIAMAuditConfig(ctx, "kms-data-access", &projects.IAMAuditConfigArgs{
			Project: pulumi.String(project),
			Service: pulumi.String("cloudkms.googleapis.com"),
			AuditLogConfigs: projects.IAMAuditConfigAuditLogConfigArray{
				&projects.IAMAuditConfigAuditLogConfigArgs{LogType: pulumi.String("DATA_READ")},
				&projects.IAMAuditConfigAuditLogConfigArgs{LogType: pulumi.String("DATA_WRITE")},
			},
		})
		if err != nil {
			return err
		}

		// Backups and point-in-time recovery stay on, sized for disaster
		// recovery and nothing else. Deleting a tenant no longer means editing
		// them: their rows are encrypted under versions that will be gone.
		db, err := sql.NewDatabaseInstance(ctx, "sessions", &sql.DatabaseInstanceArgs{
			DatabaseVersion: pulumi.String("POSTGRES_17"),
			Region:          pulumi.String(region),
			Settings: &sql.DatabaseInstanceSettingsArgs{
				Tier: pulumi.String("db-custom-1-3840"),
				BackupConfiguration: &sql.DatabaseInstanceSettingsBackupConfigurationArgs{
					Enabled:                     pulumi.Bool(true),
					PointInTimeRecoveryEnabled:  pulumi.Bool(true),
					TransactionLogRetentionDays: pulumi.Int(7),
					BackupRetentionSettings: &sql.DatabaseInstanceSettingsBackupConfigurationBackupRetentionSettingsArgs{
						RetainedBackups: pulumi.Int(30),
					},
				},
			},
			DeletionProtection: pulumi.Bool(true),
		})
		if err != nil {
			return err
		}

		ctx.Export("keyRing", ring.ID())
		ctx.Export("database", db.ConnectionName)
		ctx.Export("run", pulumi.Sprintf("go run . -kms gcp -keyring %s write", ring.ID()))
		return nil
	})
}
