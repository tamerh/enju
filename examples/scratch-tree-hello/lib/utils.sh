# Library sourced by entry.sh via `. ./lib/utils.sh`.
# Proves cross-directory reachability against the snapshot CWD.
greet() {
	echo "hello from a script that found its siblings"
}
