#!/bin/sh
set -e

# Render the YAML config from a template so deployment-scoped values
# (database host, upstream URLs) can be injected through the environment.
if [ -n "${CONFIG_TEMPLATE:-}" ] && [ -n "${RENDERED_CONFIG:-}" ] && [ -f "${CONFIG_TEMPLATE}" ]; then
  envsubst < "${CONFIG_TEMPLATE}" > "${RENDERED_CONFIG}"
fi

exec "$@"
