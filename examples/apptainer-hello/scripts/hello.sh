#!/bin/sh
# Apptainer smoke-test script. Runs inside the container; the
# wrapper bind-mounts the project workspace at /workspace and
# sets $ENJU_PROJECT_DIR to that path so this script doesn't
# need to know its host-side mount point.

set -e

# Read the container's OS id to prove this ran inside alpine
# rather than falling through to a host-side exec. Alpine's
# /etc/os-release exports ID=alpine and VERSION_ID, so the
# echo'd line is unambiguous.
. /etc/os-release
echo "hello from $ID $VERSION_ID"

# Write the declared artifact. writes in the manifest
# (greetings/hello.txt) tells Enju to pick this up from the
# workspace after the script exits and commit it to the run
# branch.
mkdir -p "$ENJU_PROJECT_DIR/greetings"
echo "hello from $ID $VERSION_ID" > "$ENJU_PROJECT_DIR/greetings/hello.txt"
