#!/bin/sh
# Entry script — runs with the template-snapshot tree as CWD.
# Reach siblings naturally via relative paths.
set -e

# Source a library from a sibling subdirectory.
. ./lib/utils.sh

# Invoke a helper script from another sibling subdirectory.
./scripts/helper.sh

# The greet function (defined in lib/utils.sh) writes its line
# to stdout, which becomes the task's result.md content.
greet

# Outputs that need to be persisted as artifacts go via
# $ENJU_SCRATCH (writable per-iter sandbox). The wrapper will
# pick up declared writes from the workspace after
# the script exits.
echo "synthesis complete" > "${ENJU_SCRATCH}/greeting.txt"
