#!/usr/bin/env bash
set -e

admin/build.sh

sudo dpkg -i builds/omogenjudge-sandbox-dev.deb
