# crypto-shredding

Agent session events in Postgres, encrypted with one data key per tenant per month, each wrapped by its own key version in a KMS. Deleting is destroying key versions: a whole tenant (`offboard`) or every month past retention (`expire`). A backup taken before the deletion keeps only what was not shredded readable.

`-kms local` (the default) runs everything on your machine: a local stand-in for Cloud KMS keeps its keys in `kms/keys.json`, never in the database. `-kms gcp -keyring projects/<p>/locations/<l>/keyRings/<r>` uses Cloud KMS, with one CryptoKey per tenant and one version per month.

## Run it

```sh
docker compose up -d --wait
go run . reset
go run . write

# a backup, taken before any deletion
docker compose exec -T db pg_dump -U postgres sessions > backup.sql
grep -c hunter2 backup.sql   # 0: the secrets in it are encrypted

go run . offboard globex                    # destroys every globex key version
go run . expire -keep 1 -now 2026-11-15     # keeps October and November, shreds September
go run . read                               # the live database

# restore the old backup next to it and read it with the same keys
docker compose exec -T db createdb -U postgres restored
docker compose exec -T db psql -q -U postgres -d restored < backup.sql
DATABASE_URL=postgres://postgres:postgres@localhost:5432/restored go run . read
```

In the restored backup, every globex event and acme's September come back `[shredded]`; acme's October and November are still readable.

## Tests and benchmarks

```sh
docker compose exec -T db createdb -U postgres test
TEST_DATABASE_URL=postgres://postgres:postgres@localhost:5432/test go test -bench . .
```

## infra

A Pulumi Go stack for the GCP side: the key ring, an application identity that can create and use keys but never destroy them, a retention identity that can only destroy, KMS data access logs, and a Cloud SQL Postgres with backups and point-in-time recovery. Per-tenant keys are created by the application at runtime, not declared in the stack.

```sh
cd infra
pulumi config set gcp:project <project>
pulumi up
```
