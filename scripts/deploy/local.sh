#!/usr/bin/env bash
set -e

admin/build.sh

sudo dpkg -i builds/omogenjudge-local-dev.deb
