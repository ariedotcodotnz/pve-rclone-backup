# SPDX-License-Identifier: AGPL-3.0-or-later
#
# The plugin must load through the real PVE::Storage loader, register its
# type and never collide with property names of other storage plugins: a
# duplicate property makes PVE::Storage die on the whole node.
use v5.36;

use FindBin;
use Test::More;

my @warnings;
BEGIN { $SIG{__WARN__} = sub { push @warnings, @_ } }

use PVE::Storage;

my @load_problems = grep { /rclone|Error loading storage plugin/i } @warnings;
is_deeply(\@load_problems, [], 'plugin loads without warnings');

my $plugin = eval { PVE::Storage::Plugin->lookup('rclone-backup') };
is($plugin, 'PVE::Storage::Custom::RcloneBackupPlugin', 'rclone-backup type registered');

my $apiver = PVE::Storage::APIVER();
my $api = $plugin->api();
cmp_ok($api, '>=', $apiver - PVE::Storage::APIAGE(), 'api() within PVE APIAGE window');
is($api, $apiver, "api() reports running APIVER $apiver")
    if $apiver >= $plugin->APIVER_MIN && $apiver <= $plugin->APIVER_MAX;

my $props = $plugin->properties();
ok(scalar(keys %$props) > 10, 'properties defined');
for my $p (sort keys %$props) {
    like($p, qr/^rclone-[a-z0-9-]+$/, "property '$p' carries the rclone- prefix");
    ok(length($props->{$p}->{title} // ''), "property '$p' has a title");
    ok(length($props->{$p}->{description} // ''), "property '$p' has a description");
}

open(my $fh, '<', "$FindBin::Bin/data/third-party-properties.txt") or die $!;
my @foreign = grep { length && !/^#/ } map { chomp; $_ } <$fh>;
close($fh);
for my $name (@foreign) {
    ok(!exists($props->{$name}), "no collision with third-party property '$name'");
}

my $options = $plugin->options();
for my $opt (sort keys %$options) {
    ok(exists($props->{$opt}) || exists(PVE::Storage::Plugin->private()->{propertyList}->{$opt}),
        "option '$opt' refers to a known property");
}

ok(PVE::Storage::Plugin::storage_has_feature('rclone-backup', 'backup-provider'),
    'declares the backup-provider feature');
is_deeply(PVE::Storage::Plugin::sensitive_properties('rclone-backup'), [],
    'declares no sensitive properties');

done_testing();
