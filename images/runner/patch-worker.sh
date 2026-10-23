#!/bin/sh
# Points a GitHub Actions runner's jobs at a configured Actions cache server
# (README "Using the cache server").
#
# The runner's Worker sets ACTIONS_RESULTS_URL for JavaScript and container
# actions from the job's service connection, after it copies the job's env
# context in, so a value from the env context never reaches those actions.
# The variable name is a UTF-16LE string literal in Runner.Worker.dll.
# Renaming that literal to ACTIONS_RESULTS_ORL (same length) makes the Worker
# store GitHub's URL under a name nothing reads; the job-started hook then
# sets ACTIONS_RESULTS_URL in the env context, and every step sees that.
#
# The image ships the Worker unpatched. pre-listener.sh, sourced through
# BASH_ENV at every run-helper.sh start, runs "apply" on the Worker that bin/
# names, so a self-update's fresh copy is patched before the job too.
#
# Usage:
#   patch-worker.sh count DLL     print "<original> <patched>" occurrence counts
#   patch-worker.sh check N DLL   succeed iff DLL is unpatched with exactly N
#                                 (N > 0) occurrences; the image build pins N
#   patch-worker.sh apply DLL     patch in place; succeed if DLL ends up
#                                 patched, fail on any unexpected state
# DLL may be reached through a symlink.
set -eu

prog=lux-gha-patch-worker

# "apply" writes back the same inode ("+<"), so owner, mode and any symlink
# to the file are kept.
worker() {
	perl -e '
		my ($mode, $f) = @ARGV;
		my $u = sub { join "", map { $_ . "\0" } split //, shift };
		my $from = $u->("ACTIONS_RESULTS_URL");
		my $to = $u->("ACTIONS_RESULTS_ORL");
		open(my $fh, ($mode eq "apply" ? "+<" : "<") . ":raw", $f) or die "cannot open $f: $!\n";
		local $/; my $d = <$fh>;
		my $n = () = $d =~ /\Q$from\E/g;
		my $m = () = $d =~ /\Q$to\E/g;
		if ($mode eq "count") { print "$n $m\n"; exit 0 }
		if ($n == 0 && $m > 0) { print "already patched ($m)\n"; exit 0 }
		die "unexpected state: $n original and $m patched occurrences\n" if $n == 0 || $m != 0;
		(my $p = $d) =~ s/\Q$from\E/$to/g;
		my $n2 = () = $p =~ /\Q$from\E/g;
		my $m2 = () = $p =~ /\Q$to\E/g;
		die "patch did not verify\n" unless length($p) == length($d) && $n2 == 0 && $m2 == $n;
		seek($fh, 0, 0) or die "seek: $!\n";
		print $fh $p or die "write: $!\n";
		close($fh) or die "close: $!\n";
		print "patched ($n)\n";
	' "$1" "$2"
}

die() { echo "$prog: $*" >&2; exit 1; }

[ $# -ge 1 ] || die "usage: patch-worker.sh count DLL | check N DLL | apply DLL"
cmd=$1
shift
case $cmd in
count)
	[ $# -eq 1 ] || die "usage: patch-worker.sh count DLL"
	[ -f "$1" ] || die "$1: not found"
	worker count "$1"
	;;
check)
	[ $# -eq 2 ] || die "usage: patch-worker.sh check N DLL"
	case $1 in '' | *[!0-9]* | 0) die "N must be a positive number" ;; esac
	[ -f "$2" ] || die "$2: not found"
	c=$(worker count "$2") || die "$2: cannot count"
	[ "$c" = "$1 0" ] || die "$2: expected \"$1 0\" (original, patched) occurrences of ACTIONS_RESULTS_URL, found \"$c\"; update the pin with the runner version"
	;;
apply)
	[ $# -eq 1 ] || die "usage: patch-worker.sh apply DLL"
	[ -f "$1" ] || die "$1: not found"
	out=$(worker apply "$1" 2>&1) || die "$1: $out"
	echo "$prog: $out"
	;;
*) die "unknown command $cmd" ;;
esac
