# SPDX-License-Identifier: AGPL-3.0-or-later
#
# storage.cfg parsing and validation through PVE's SectionConfig code.
use v5.36;

use FindBin;
use Test::More;

use PVE::Storage;

my $plugin = 'PVE::Storage::Custom::RcloneBackupPlugin';

open(my $fh, '<', "$FindBin::Bin/data/storage.cfg") or die $!;
my $raw = do { local $/; <$fh> };
close($fh);

my @warnings;
my $cfg = do {
    local $SIG{__WARN__} = sub { push @warnings, @_ };
    PVE::Storage::Plugin->parse_config('storage.cfg', $raw);
};
is_deeply(\@warnings, [], 'fixture parses without warnings');

my $scfg = $cfg->{ids}->{offsite};
is($scfg->{type}, 'rclone-backup', 'type');
is($scfg->{'rclone-remote'}, 'onedrive-main', 'remote');
is($scfg->{'rclone-source'}, 'homelab', 'source');
is($scfg->{'rclone-tags'}, 'offsite;critical', 'tag list');
is_deeply($scfg->{content}, { backup => 1 }, 'content');
ok($scfg->{shared}, 'shared');
is($cfg->{ids}->{local}->{type}, 'dir', 'other storages unaffected');

my $base = { type => 'rclone-backup', 'rclone-remote' => 'od', 'rclone-source' => 'homelab' };

sub check($extra, $create = 1) {
    return eval { $plugin->check_config('offsite', { %$base, %$extra }, $create, 0) };
}

sub error_keys() {
    my $err = $@;
    return '' if !$err;
    return join(',', sort keys %{ $err->{errors} }) if ref($err) && ref($err->{errors}) eq 'HASH';
    return "$err";
}

my $opts = check({});
ok($opts, 'minimal config accepted') or diag(error_keys());
is($opts->{shared}, 1, 'shared forced on create');

for my $case (
    ['rclone-transfers', 99],
    ['rclone-transfers', 0],
    ['rclone-remote', 'Bad Name'],
    ['rclone-source', '-bad'],
    ['rclone-path', '../escape'],
    ['rclone-path', '/absolute'],
    ['rclone-encryption', 'aes'],
    ['rclone-min-interval', '7 days'],
    ['rclone-segment-size', '1GiB'],
    ['rclone-tags', 'bad tag!'],
    ['rclone-vmids', 'abc'],
    ['rclone-replicate-from', 'bad storage!'],
    ['rclone-verify-content', 'sometimes'],
) {
    my ($key, $value) = @$case;
    ok(!check({ $key => $value }), "rejects $key=$value");
}

for my $case (
    ['rclone-path', 'pve/backups-2'],
    ['rclone-tags', 'offsite;critical'],
    ['rclone-vmids', '100,101,200'],
    ['rclone-replicate-from', 'local,nas-backup'],
    ['rclone-min-interval', '0'],
    ['rclone-verify-content', 'off'],
    ['rclone-bwlimit', '08:00,1M 23:00,off'],
    ['prune-backups', 'keep-daily=7,keep-weekly=4'],
) {
    my ($key, $value) = @$case;
    ok(check({ $key => $value }), "accepts $key=$value") or diag(error_keys());
}

ok(!check({ 'rclone-guests' => 'listed' }), "guests=listed without vmids rejected");
ok(check({ 'rclone-guests' => 'listed', 'rclone-vmids' => '100' }), "guests=listed with vmids accepted");

done_testing();
