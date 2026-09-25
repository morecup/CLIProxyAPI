#!/usr/bin/env bash
set -euo pipefail

models_repository="${MODELS_REPOSITORY_URL:-https://github.com/router-for-me/models.git}"
models_ref="${MODELS_REPOSITORY_REF:-main}"
catalog_dir="${MODEL_CATALOG_DIR:-internal/registry/models}"

git fetch --depth 1 "$models_repository" "$models_ref"
git show FETCH_HEAD:models.json > "$catalog_dir/models.json"
