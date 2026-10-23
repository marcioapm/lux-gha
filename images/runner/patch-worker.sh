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
# A self-update replaces bin/ with a fresh, unpatched copy (bin becomes a
# symlink to bin.<version>) and restarts the listener before it takes the
# job. So the image never patches at build time: run-helper.sh.template,
# which run.sh copies and runs before every listener start, applies the patch
# when the Run sets LUXGHA_ACTIONS_RESULTS_URL.
#
# Usage:
#   patch-worker.sh count DLL     print "<original> <patched>" occurrence counts
#   patch-worker.sh check N DLL   succeed iff DLL is unpatched with exactly N
#                                 occurrences (the image build pins N)
#   patch-worker.sh apply DLL     patch in place; succeed if DLL ends up
#                                 patched, fail on any unexpected state
#   patch-worker.sh helper FILE   insert the pre-start patch step into a
#                                 run-helper.sh.template, once
# DLL may be reached through a symlink.
set -eu

prog=lux-gha-patch-worker
marker='# lux-gha: point jobs at the Actions cache server (patch-worker.sh)'
# The listener line of run-helper.sh.template, literally.
# shellcheck disable=SC2016
anchor='"$DIR"/bin/Runner.Listener run $*'

# Every mode reads the whole file and writes back the same inode ("+<"), so
# owner, mode and any symlink to the file are kept.
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

# The step runs under run-helper's bash before the listener starts. A failed
# patch leaves the Worker as GitHub ships it; the job-started hook sees that
# and warns, and the job runs against GitHub's cache service.
helper() {
	perl -e '
		my ($f, $marker, $anchor) = @ARGV;
		open(my $fh, "+<:raw", $f) or die "cannot open $f: $!\n";
		local $/; my $d = <$fh>;
		if (index($d, $marker) >= 0) { print "already present\n"; exit 0 }
		my $k = () = $d =~ /^\Q$anchor\E$/mg;
		die "expected one line \"$anchor\", found $k\n" unless $k == 1;
		my $step = join "", map { "$_\n" } (
			$marker,
			"if [ -n \"\${LUXGHA_ACTIONS_RESULTS_URL:-}\" ]; then",
			"    \"\${LUXGHA_PATCH_WORKER:-/usr/local/lib/lux-gha/patch-worker.sh}\" apply \"\$DIR\"/bin/Runner.Worker.dll ||",
			"        echo \"lux-gha: warning: Runner.Worker.dll is not patched; jobs use GitHub\x27s cache service\" >&2",
			"fi",
		);
		$d =~ s/^(\Q$anchor\E)$/$step$1/m;
		seek($fh, 0, 0) or die "seek: $!\n";
		print $fh $d or die "write: $!\n";
		truncate($fh, tell($fh)) or die "truncate: $!\n";
		close($fh) or die "close: $!\n";
		print "inserted\n";
	' "$1" "$marker" "$anchor"
}

die() { echo "$prog: $*" >&2; exit 1; }

[ $# -ge 1 ] || die "usage: patch-worker.sh count|check N|apply|helper FILE"
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
	case $1 in '' | *[!0-9]*) die "N must be a number" ;; esac
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
helper)
	[ $# -eq 1 ] || die "usage: patch-worker.sh helper FILE"
	[ -f "$1" ] || die "$1: not found"
	out=$(helper "$1" 2>&1) || die "$1: $out"
	echo "$prog: $out"
	;;
*) die "unknown command $cmd" ;;
esac
