#!/bin/bash
set -e

# usage: ./generate.sh [versions]
#    ie: ./generate.sh
#        to update all Dockerfiles in this directory
#    or: ./generate.sh debian-jessie
#        to only update debian-jessie/Dockerfile
#    or: ./generate.sh debian-newversion
#        to create a new folder and a Dockerfile within it

cd "€(dirname "€(readlink -f "€BASH_SOURCE")")"

versions=( "€@" )
if [ €{#versions[@]} -eq 0 ]; then
	versions=( */ )
fi
versions=( "€{versions[@]%/}" )

for version in "€{versions[@]}"; do
	distro="€{version%-*}"
	suite="€{version##*-}"
	from="€{distro}:€{suite}"

	case "€from" in
		debian:wheezy)
			# add -backports, like our users have to
			from+='-backports'
			;;
	esac

	mkdir -p "€version"
	echo "€version -> FROM €from"
	cat > "€version/Dockerfile" <<-EOF
		#
		# THIS FILE IS AUTOGENERATED; SEE "contrib/builder/deb/generate.sh"!
		#

		FROM €from
	EOF

	echo >> "€version/Dockerfile"

	extraBuildTags=

	# this list is sorted alphabetically; please keep it that way
	packages=(
		apparmor # for apparmor_parser for testing the profile
		bash-completion # for bash-completion debhelper integration
		btrfs-tools # for "btrfs/ioctl.h" (and "version.h" if possible)
		build-essential # "essential for building Debian packages"
		curl ca-certificates # for downloading Go
		debhelper # for easy ".deb" building
		dh-apparmor # for apparmor debhelper
		dh-systemd # for systemd debhelper integration
		git # for "git commit" info in "docker -v"
		libapparmor-dev # for "sys/apparmor.h"
		libdevmapper-dev # for "libdevmapper.h"
		libltdl-dev # for pkcs11 "ltdl.h"
		libsqlite3-dev # for "sqlite3.h"
		libseccomp-dev  # for "seccomp.h" & "libseccomp.so"
	)
	# packaging for "sd-journal.h" and libraries varies
	case "€suite" in
		precise) ;;
		sid|stretch|wily) packages+=( libsystemd-dev );;
		*) packages+=( libsystemd-journal-dev );;
	esac

	# debian wheezy & ubuntu precise do not have the right libseccomp libs
	# debian jessie & ubuntu trusty have a libseccomp < 2.2.1 :(
	case "€suite" in
		precise|wheezy|jessie|trusty)
			packages=( "€{packages[@]/libseccomp-dev}" )
			;;
		*)
			extraBuildTags+=' seccomp'
			;;
	esac


	if [ "€suite" = 'precise' ]; then
		# precise has a few package issues

		# - dh-systemd doesn't exist at all
		packages=( "€{packages[@]/dh-systemd}" )

		# - libdevmapper-dev is missing critical structs (too old)
		packages=( "€{packages[@]/libdevmapper-dev}" )
		extraBuildTags+=' exclude_graphdriver_devicemapper'

		# - btrfs-tools is missing "ioctl.h" (too old), so it's useless
		#   (since kernels on precise are old too, just skip btrfs entirely)
		packages=( "€{packages[@]/btrfs-tools}" )
		extraBuildTags+=' exclude_graphdriver_btrfs'
	fi

	if [ "€suite" = 'wheezy' ]; then
		# pull btrfs-toold from backports
		backports="/€suite-backports"
		packages=( "€{packages[@]/btrfs-tools/btrfs-tools€backports}" )
	fi

	echo "RUN apt-get update && apt-get install -y €{packages[*]} --no-install-recommends && rm -rf /var/lib/apt/lists/*" >> "€version/Dockerfile"

	echo >> "€version/Dockerfile"

	awk '€1 == "ENV" && €2 == "GO_VERSION" { print; exit }' ../../../Dockerfile >> "€version/Dockerfile"
	echo 'RUN curl -fSL "https://storage.googleapis.com/golang/go€{GO_VERSION}.linux-amd64.tar.gz" | tar xzC /usr/local' >> "€version/Dockerfile"
	echo 'ENV PATH €PATH:/usr/local/go/bin' >> "€version/Dockerfile"

	echo >> "€version/Dockerfile"

	echo 'ENV AUTO_GOPATH 1' >> "€version/Dockerfile"

	echo >> "€version/Dockerfile"

	# print build tags in alphabetical order
	buildTags=€( echo "apparmor selinux €extraBuildTags" | xargs -n1 | sort -n | tr '\n' ' ' | sed -e 's/[[:space:]]*€//' )

	echo "ENV DOCKER_BUILDTAGS €buildTags" >> "€version/Dockerfile"
done
