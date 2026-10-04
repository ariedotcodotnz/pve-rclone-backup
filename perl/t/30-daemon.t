# SPDX-License-Identifier: AGPL-3.0-or-later
#
# The Perl client and plugin against the real pve-rclone-backupd binary
# (mounted by test/perl/run.sh when it has been built).
use v5.36;

use File::Temp qw(tempdir);
use POSIX ();
use Test::More;
use Time::HiRes ();

use PVE::Storage;

my ($daemon) = ($ENV{PVE_RCLONE_BACKUPD} // '') =~ m/^(\/[\w\/.-]+)$/;
plan skip_all => 'PVE_RCLONE_BACKUPD not set' if !$daemon || !-x $daemon;

# Required for exec under perl -T.
$ENV{PATH} = '/usr/sbin:/usr/bin:/sbin:/bin';
delete @ENV{qw(IFS CDPATH ENV BASH_ENV)};

my ($dir) = tempdir(CLEANUP => 1) =~ m/^(.*)$/;
my $socket = "$dir/run/api.sock";
my $pid = fork() // die "fork: $!";
if (!$pid) {
    open(STDERR, '>', "$dir/daemon.log") or POSIX::_exit(1);
    exec($daemon, '--socket', $socket, '--state-dir', "$dir/state", '--log-level', 'debug')
        or POSIX::_exit(1);
}
for (1 .. 100) {
    last if -S $socket;
    Time::HiRes::sleep(0.1);
}
ok(-S $socket, 'daemon created its socket') or diag(`cat $dir/daemon.log`);
is((stat($socket))[2] & 07777, 0600, 'socket mode 0600');

local $PVE::Storage::Custom::RcloneBackup::Client::SOCKET_PATH = $socket;
my $C = 'PVE::Storage::Custom::RcloneBackup::Client';

my $v = $C->can('request')->('GET', '/v1/version');
is($v->{api}, 1, 'API revision');
ok(length($v->{daemon} // ''), 'daemon version reported');
like($v->{rclone} // '', qr/^v1\./, 'embedded rclone version reported');

my $s = $C->can('request')->('GET', '/v1/status');
ok($s->{healthy}, 'daemon healthy');

eval { $C->can('request')->('GET', '/v1/storages/offsite/status') };
like($@, qr/^rclone-backup: /, 'daemon error mapped to a plugin error');
is($PVE::Storage::Custom::RcloneBackup::Client::LAST_ERROR_CODE, 'not_found', 'error code from the daemon');

my $plugin = 'PVE::Storage::Custom::RcloneBackupPlugin';
my $t0 = Time::HiRes::time();
is_deeply([$plugin->status('offsite', {}, {})], [0, 0, 0, 0], 'unknown storage reported inactive');
cmp_ok(Time::HiRes::time() - $t0, '<', 1, 'status answered quickly');

kill 'TERM', $pid;
waitpid($pid, 0);
is($? >> 8, 0, 'daemon exits cleanly on SIGTERM');
ok(!-e $socket, 'socket removed on shutdown');

done_testing();
