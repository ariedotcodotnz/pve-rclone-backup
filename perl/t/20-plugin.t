# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Plugin and provider behaviour against a fake daemon, including genuine
# PVE::Storage entry points (volume_list, extract_vzdump_config,
# prune_backups) that dispatch to the plugin.
use v5.36;

use FindBin;
my $bin;
BEGIN { ($bin) = $FindBin::Bin =~ m/^(.*)$/ } # untaint for perl -T
use lib "$bin/lib";

use Test::More;
use Time::HiRes ();

use FakeDaemon;
use PVE::Storage;

my $plugin = 'PVE::Storage::Custom::RcloneBackupPlugin';
my $vol = 'backup/vzdump-qemu-100-2026_10_04-02_00_01.vma.zst';
my $ctvol = 'backup/vzdump-lxc-201-2026_10_04-03_00_01.tar.zst';

my $cfg = do {
    open(my $fh, '<', "$FindBin::Bin/data/storage.cfg") or die $!;
    local $/;
    PVE::Storage::Plugin->parse_config('storage.cfg', <$fh>);
};
my $scfg = $cfg->{ids}->{offsite};

my $d = FakeDaemon->new(routes => {
    'GET /v1/storages/offsite' => sub ($req) {
        return (200, { storage => 'offsite', repo_uuid => '6f0c2f1e-3a7b-4c2d-9e8f-0123456789ab' });
    },
    'GET /v1/storages/offsite/status' => sub ($req) {
        return (200, { total => 1099511627776, avail => 549755813888, used => 549755813888, active => JSON::true });
    },
    'GET /v1/storages/offsite/backups' => sub ($req) {
        return (200, [
            { volname => $vol, vmtype => 'qemu', vmid => 100, size => 21474836480, ctime => 1790992801,
              notes => 'web01 nightly', protected => JSON::true },
            { volname => $ctvol, vmtype => 'lxc', vmid => 201, size => 1048576, ctime => 1790996401 },
            { volname => 'backup/../../etc/passwd', vmtype => 'qemu' },
            { volname => "backup/vzdump-qemu-100-2026_10_04-02_00_01.vma.zst; rm -rf /" },
        ]);
    },
    'GET /v1/storages/offsite/backups/vzdump-qemu-100-2026_10_04-02_00_01.vma.zst' => sub ($req) {
        return (200, { volname => $vol, size => 21474836480, ctime => 1790992801, notes => 'web01 nightly',
            protected => JSON::true });
    },
    'GET /v1/storages/offsite/backups/vzdump-qemu-100-2026_10_04-02_00_01.vma.zst/config' => sub ($req) {
        return (200, { config => "name: web01\nmemory: 2048\n", firewall => undef, firewall_known => JSON::true });
    },
    'PATCH /v1/storages/offsite/backups/vzdump-qemu-100-2026_10_04-02_00_01.vma.zst' => sub ($req) {
        return (200, {});
    },
    'DELETE /v1/storages/offsite/backups/vzdump-qemu-100-2026_10_04-02_00_01.vma.zst' => sub ($req) {
        return (200, { state => 'tombstoned' });
    },
    'POST /v1/storages/offsite/prune' => sub ($req) {
        return (200, [
            { volid => "offsite:$vol", ctime => 1790992801, type => 'qemu', vmid => 100, mark => 'keep' },
            { volid => 'offsite:backup/vzdump-qemu-100-2026_09_01-02_00_01.vma.zst', ctime => 1788228001,
              type => 'qemu', vmid => 100, mark => 'remove' },
        ]);
    },
    'POST /v1/storages/offsite/validate' => sub ($req) { return (200, { ok => JSON::true }) },
    # Storages being added: not served yet, one valid and one not.
    'POST /v1/storages/added/validate' => sub ($req) { return (200, { ok => JSON::true }) },
    'POST /v1/storages/broken/validate' => sub ($req) {
        return (412, { error => { code => 'precondition_failed', message => 'no repository is known' } });
    },
});
local $PVE::Storage::Custom::RcloneBackup::Client::SOCKET_PATH = $d->socket_path;

# Volume names and paths.
is_deeply([$plugin->parse_volname($vol)],
    ['backup', 'vzdump-qemu-100-2026_10_04-02_00_01.vma.zst', 100, undef, undef, undef, 'vma.zst'],
    'parse_volname');
ok(eval { $plugin->parse_volname('backup/vzdump-qemu-100-2026_10_04-02_00_01.2.vma.zst') },
    'collision suffix accepted');
for my $bad ('backup/foo', 'images/100/vm-100-disk-0.raw', 'backup/vzdump-qemu-100-2026_10_04-02_00_01.vma.zst/../x',
    'backup/vzdump-qemu-99-2026_10_04-02_00_01.vma.zst') {
    ok(!eval { $plugin->parse_volname($bad) }, "rejects volname '$bad'");
}
my ($path, $owner, $vtype) = $plugin->path($scfg, $vol, 'offsite');
is($path, '/run/pve-rclone-backup/virtual/offsite/vzdump-qemu-100-2026_10_04-02_00_01.vma.zst',
    'path() is virtual');
is($owner, 100, 'path() owner');
is($vtype, 'backup', 'path() vtype');

# Status and activation.
is_deeply([$plugin->status('offsite', $scfg, {})], [1099511627776, 549755813888, 549755813888, 1], 'status');
ok($plugin->activate_storage('offsite', $scfg, {}), 'activate_storage');
ok($plugin->check_connection('offsite', $scfg), 'check_connection');
# PVE activates a storage being added before storage.cfg names it.
ok($plugin->activate_storage('added', $scfg, {}), 'activating a storage being added validates it');
ok($plugin->check_connection('added', $scfg), 'a valid storage being added is online');
ok(!eval { $plugin->activate_storage('broken', $scfg, {}) }, 'an invalid storage being added fails to activate');
like($@, qr/no repository is known/, 'with the reason');
ok(!$plugin->check_connection('broken', $scfg), 'an invalid storage being added is offline');
is($plugin->get_identity($scfg, 'offsite'), '6f0c2f1e-3a7b-4c2d-9e8f-0123456789ab', 'get_identity');

# Listing through PVE::Storage.
my $list = PVE::Storage::volume_list($cfg, 'offsite', undef, 'backup');
is(scalar(@$list), 2, 'invalid volume names from the daemon are dropped');
my ($vm) = grep { $_->{vmid} == 100 } @$list;
is($vm->{volid}, "offsite:$vol", 'volid');
is($vm->{subtype}, 'qemu', 'subtype drives the GUI restore dialog');
is($vm->{format}, 'vma.zst', 'format');
is($vm->{protected}, 1, 'protected');
is($vm->{notes}, 'web01 nightly', 'notes');
my ($ct) = grep { $_->{vmid} == 201 } @$list;
is($ct->{subtype}, 'lxc', 'container subtype');
is_deeply($plugin->list_volumes('offsite', $scfg, undef, ['images']), [], 'no images content');

# Attributes, size and deletion.
is($plugin->get_volume_attribute($scfg, 'offsite', $vol, 'notes'), 'web01 nightly', 'notes attribute');
is($plugin->get_volume_attribute($scfg, 'offsite', $vol, 'protected'), 1, 'protected attribute');
is(scalar($plugin->volume_size_info($scfg, 'offsite', $vol)), 21474836480, 'volume_size_info');
$plugin->update_volume_attribute($scfg, 'offsite', $vol, 'protected', 0);
$plugin->update_volume_attribute($scfg, 'offsite', $vol, 'notes', 'changed');
ok(!eval { $plugin->update_volume_attribute($scfg, 'offsite', $vol, 'bogus', 1) }, 'unknown attribute rejected');
is($plugin->free_image('offsite', $scfg, $vol), undef, 'free_image returns no cleanup worker');

# Prune through PVE::Storage.
my @log;
my $pruned = PVE::Storage::prune_backups($cfg, 'offsite', { 'keep-daily' => 7 }, 100, 'qemu', 0,
    sub { push @log, [@_] });
is(scalar(@$pruned), 2, 'prune list returned');
is_deeply([map { $_->{mark} } @$pruned], ['keep', 'remove'], 'prune marks');
is(scalar(grep { $_->[1] =~ /tombstoning/ } @log), 1, 'removal logged');

# Configuration extraction through PVE::Storage (the GUI's "Show Configuration").
is(PVE::Storage::extract_vzdump_config($cfg, "offsite:$vol"), "name: web01\nmemory: 2048\n",
    'extract_vzdump_config served from the daemon');
my $provider = $plugin->new_backup_provider($scfg, 'offsite', sub { });
isa_ok($provider, 'PVE::BackupProvider::Plugin::Base');
is($provider->archive_get_firewall_config($vol), undef, 'no firewall config');
like(eval { $provider->job_init(time()) } // $@, qr/direct backups to an rclone-backup storage are not supported/,
    'direct backups refused with guidance');
like(eval { $provider->restore_get_mechanism($vol) } // $@, qr/pve-rclone-backup restore/,
    'GUI restore points to the CLI');

# Hooks validate via the daemon.
ok(!defined($plugin->on_add_hook('offsite', $scfg)), 'on_add_hook validates');

my %by = map { ("$_->{method} $_->{path}" => $_) } @{ $d->requests };
is($by{'GET /v1/storages/offsite/backups'}->{query}, '', 'no vmid filter when listing everything');
my @patches = grep { $_->{method} eq 'PATCH' } @{ $d->requests };
is_deeply([map { $_->{body} } @patches], [{ protected => JSON::false }, { notes => 'changed' }], 'PATCH bodies');
is_deeply($by{'POST /v1/storages/offsite/prune'}->{body},
    { keep => { 'keep-daily' => 7 }, vmid => 100, type => 'qemu', dry_run => JSON::false }, 'prune request');
$d->stop;

# A dead daemon makes the storage inactive without stalling pvestatd.
{
    local $PVE::Storage::Custom::RcloneBackup::Client::SOCKET_PATH = '/nonexistent/api.sock';
    my $t0 = Time::HiRes::time();
    is_deeply([$plugin->status('offsite', $scfg, {})], [0, 0, 0, 0], 'status inactive when daemon is down');
    cmp_ok(Time::HiRes::time() - $t0, '<', 1, 'status returns quickly');
    ok(!eval { $plugin->activate_storage('offsite', $scfg, {}) }, 'activation fails when daemon is down');
    like($@, qr/pve-rclone-backupd running/, 'activation error explains the cause');
}

done_testing();
