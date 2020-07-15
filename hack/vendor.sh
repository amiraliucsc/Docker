#!/usr/bin/env bash

# This file is just wrapper around vndr (github.com/LK4D4/vndr) tool.
# For updating dependencies you should change `vendor.conf` file in root of the
# project. Please refer to https://github.com/LK4D4/vndr/blob/master/README.md for
# vndr usage.

set -e

if ! hash vndr; then
	echo "Please install vndr with \"go get github.com/LK4D4/vndr\" and put it in your \$GOPATH"
	exit 1
fi

if [ $# -eq 0 ]; then
	# update our vendored copy of archive/tar when doing a full vendor
	GO_VERSION="${GO_VERSION:=$(grep "ARG GO_VERSION" ./Dockerfile | awk -F'=' '{print $2}')}"
	rm -rf vendor/archive
	mkdir -p ./vendor/archive
	git clone -b go"${GO_VERSION}" --depth=1 git://github.com/golang/go.git ./go
	git -c user.email="moby@example.com" -c user.name="moby" --git-dir ./go/.git --work-tree ./go am ../patches/0001-archive-tar-do-not-populate-user-group-names.patch
	cp -a go/src/archive/tar ./vendor/archive/tar
	rm -rf ./go
fi

vndr -whitelist=^archive/tar "$@"
