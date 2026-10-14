#!/bin/sh
set -e

# Local `make docker-run` has no bucket; run without backups.
if [ -z "$S3_BUCKET" ]; then
  exec /djm
fi

# A fresh volume gets the latest backup. An existing database is left alone.
litestream restore -if-db-not-exists -if-replica-exists "$DB_PATH"

# Litestream runs the app as its child and replicates until it exits.
exec litestream replicate -exec /djm
