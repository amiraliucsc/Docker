#!/bin/bash
set -eu

if ! command -v quicktls 2>&1 /dev/null; then
(
	cd "$(mktemp -d)"
	GO111MODULE=on go get github.com/dmcgowan/quicktls
)
fi

SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd -P)"

# integration/testdata/https (and integration-cli/fixtures/https, which has symlinks to these files)
OUT_DIR="${SCRIPT_DIR}/../integration/testdata/https"
quicktls -exp=87648h -org=Docker -with-san -clients=1 -o "${OUT_DIR}" localhost

openssl x509 -text -noout -in "${OUT_DIR}/client-0.cert" > "${OUT_DIR}/client-cert.pem" \
 && cat "${OUT_DIR}/client-0.cert" >> "${OUT_DIR}/client-cert.pem" \
 && rm "${OUT_DIR}/client-0.cert"

openssl x509 -text -noout -in "${OUT_DIR}/localhost.cert" > "${OUT_DIR}/server-cert.pem" \
 && cat "${OUT_DIR}/localhost.cert" >> "${OUT_DIR}/server-cert.pem" \
 && rm "${OUT_DIR}/localhost.cert"

mv "${OUT_DIR}/client-0.key" "${OUT_DIR}/client-key.pem"
mv "${OUT_DIR}/localhost.key" "${OUT_DIR}/server-key.pem"
