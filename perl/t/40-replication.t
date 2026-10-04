# SPDX-License-Identifier: AGPL-3.0-or-later
#
# The whole read and edit path inside PVE's storage library: the real
# daemon replicates a container archive to a local rclone remote, set up
# with the real CLI, and the plugin and provider serve it.
use v5.36;

use File::Path qw(make_path);
use File::Temp qw(tempdir);
use POSIX ();
use Test::More;
use Time::HiRes ();

use PVE::Storage;
use PVE::BackupProvider::Plugin::Rclone;

my ($daemon) = ($ENV{PVE_RCLONE_BACKUPD} // '') =~ m/^(\/[\w\/.-]+)$/;
my ($cli) = ($ENV{PVE_RCLONE_BACKUP_CLI} // '') =~ m/^(\/[\w\/.-]+)$/;
plan skip_all => 'daemon and CLI not built' if !$daemon || !-x $daemon || !$cli || !-x $cli;

$ENV{PATH} = '/usr/sbin:/usr/bin:/sbin:/bin';
delete @ENV{qw(IFS CDPATH ENV BASH_ENV)};

my ($dir) = tempdir(CLEANUP => 1) =~ m/^(.*)$/;
my $socket = "$dir/run/api.sock";
make_path("$dir/pve/priv/lock", "$dir/pve/priv/pve-rclone-backup", "$dir/backups/dump", "$dir/stage/etc/vzdump");

sub write_file($path, $content) {
    open(my $fh, '>', $path) or die "write $path: $!";
    print $fh $content;
    close($fh) or die;
}

write_file("$dir/pve/priv/pve-rclone-backup/remotes.conf", "[lr]\ntype = local\n");
my $storage_cfg = "dir: backups\n\tpath $dir/backups\n\tcontent backup\n\n";
write_file("$dir/pve/storage.cfg", $storage_cfg);

my $pid = fork() // die "fork: $!";
if (!$pid) {
    # The local remote's repository path is relative to the daemon's cwd.
    chdir($dir) or POSIX::_exit(1);
    open(STDERR, '>', "$dir/daemon.log") or POSIX::_exit(1);
    exec($daemon, '--socket', $socket, '--state-dir', "$dir/state", '--pve-dir', "$dir/pve", '--log-level', 'debug')
        or POSIX::_exit(1);
}
for (1 .. 100) {
    last if -S $socket;
    Time::HiRes::sleep(0.1);
}
ok(-S $socket, 'daemon started') or BAIL_OUT(`cat $dir/daemon.log`);

sub cli(@args) {
    my $child = open(my $fh, '-|') // die "fork: $!";
    if (!$child) {
        open(STDERR, '>&', \*STDOUT) or POSIX::_exit(127);
        exec($cli, '--socket', $socket, @args) or POSIX::_exit(127);
    }
    my $out = do { local $/; <$fh> } // '';
    close($fh);
    return ($? >> 8, $out);
}

# Create the repository and confirm its recovery kit non-interactively.
my @init = ('storage', 'init', 'offsite', '--remote', 'lr', '--path', 'repo', '--source', 'lab',
    '--replicate-from', 'backups', '--no-pvesh');
my ($code, $out) = cli(@init, '--kit-file', "$dir/kit.txt");
is($code, 2, 'init stops until the recovery kit is confirmed') or diag($out);
my ($sum) = $out =~ m/--confirm-checksum ([0-9a-f-]+)/;
ok($sum, 'kit checksum reported');
($code, $out) = cli('recovery-kit', 'confirm', $sum);
is($code, 0, 'kit confirmed') or diag($out);
($code, $out) = cli(@init);
is($code, 0, 'init completes') or diag($out);
like($out, qr/pvesh create \/storage --storage offsite --type rclone-backup/, 'pvesh command printed');

$storage_cfg .= "rclone-backup: offsite\n\trclone-remote lr\n\trclone-path repo\n\trclone-source lab\n"
    . "\trclone-replicate-from backups\n\tcontent backup\n\tshared 1\n\n";
write_file("$dir/pve/storage.cfg", $storage_cfg);
my $scfg = PVE::Storage::Plugin->parse_config('storage.cfg', $storage_cfg)->{ids}->{offsite};
ok($scfg && $scfg->{type} eq 'rclone-backup', 'storage.cfg parsed by PVE');

# A finished container backup with notes and a task log.
my $name = 'vzdump-lxc-200-2026_10_04-02_00_01.tgz';
my $archive = "$dir/backups/dump/$name";
write_file("$dir/stage/etc/vzdump/pct.conf", "arch: amd64\nhostname: ct1\nmemory: 512\n");
is(system('tar', 'czf', $archive, '-C', "$dir/stage", './etc'), 0, 'container archive created');
write_file("$archive.notes", 'nightly ct');
write_file("$dir/backups/dump/vzdump-lxc-200-2026_10_04-02_00_01.log", "INFO: Finished Backup\n");
my $old = time() - 3600;
utime($old, $old, $archive) or die;

my $plugin = 'PVE::Storage::Custom::RcloneBackupPlugin';
local $PVE::Storage::Custom::RcloneBackup::Client::SOCKET_PATH = $socket;
my $vol = "backup/$name";
my $list = [];
for (1 .. 300) {
    $list = eval { $plugin->list_volumes('offsite', $scfg, undef, ['backup']) } // [];
    last if @$list;
    Time::HiRes::sleep(0.1);
}
is(scalar(@$list), 1, 'replicated backup listed') or diag(`cat $dir/daemon.log | tail -30`);
my $e = $list->[0] // {};
is($e->{volid}, "offsite:$vol", 'volume ID');
is($e->{vmid}, 200, 'owner');
is($e->{subtype}, 'lxc', 'guest type');
is($e->{size}, -s $archive, 'archive size');
is($e->{notes}, 'nightly ct', 'notes from the sidecar');
ok(!$e->{protected}, 'not protected');

my ($total, $avail, $used, $active) = $plugin->status('offsite', $scfg, {});
is($active, 1, 'storage active');
is(scalar($plugin->volume_size_info($scfg, 'offsite', $vol)), -s $archive, 'volume size');
like($plugin->get_identity($scfg, 'offsite'), qr/^[0-9a-f-]{36}$/, 'repository identity');
like(($plugin->path($scfg, $vol, 'offsite'))[0], qr{^/run/pve-rclone-backup/virtual/offsite/}, 'virtual path');

my $provider = $plugin->new_backup_provider($scfg, 'offsite', sub { });
like($provider->archive_get_guest_config($vol), qr/hostname: ct1/, 'guest configuration from the manifest');

my $pruned = $plugin->prune_backups($scfg, 'offsite', { 'keep-last' => 1 }, undef, undef, 1, sub { });
is_deeply([map { "$_->{volid}=$_->{mark}" } @$pruned], ["offsite:$vol=keep"], 'prune preview through PVE');

is($plugin->get_volume_attribute($scfg, 'offsite', $vol, 'notes'), 'nightly ct', 'notes attribute');
$plugin->update_volume_attribute($scfg, 'offsite', $vol, 'notes', 'kept for audit');
is($plugin->get_volume_attribute($scfg, 'offsite', $vol, 'notes'), 'kept for audit', 'notes updated');
$plugin->update_volume_attribute($scfg, 'offsite', $vol, 'protected', 1);
is($plugin->get_volume_attribute($scfg, 'offsite', $vol, 'protected'), 1, 'protected');
eval { $plugin->free_image('offsite', $scfg, $vol) };
like($@, qr/protected/, 'protected backup cannot be deleted');
$plugin->update_volume_attribute($scfg, 'offsite', $vol, 'protected', 0);
eval { $plugin->free_image('offsite', $scfg, $vol) };
is($@, '', 'deletion accepted');
$list = $plugin->list_volumes('offsite', $scfg, undef, ['backup']);
is(scalar(@$list), 0, 'deleted backup hidden from PVE');

($code, $out) = cli('backup', 'list', '--state', 'tombstoned');
like($out, qr/offsite:\Q$vol\E\s+tombstoned/, 'tombstone visible in the CLI') or diag($out);

kill 'TERM', $pid;
waitpid($pid, 0);
is($? >> 8, 0, "daemon exits cleanly") or diag(`tail -20 $dir/daemon.log`);

done_testing();
