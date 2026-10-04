# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Storage property schema of the rclone-backup storage type.
#
# NOTE: hand-maintained until milestone M4 generates this module from
# schema/storage.yaml. Every property defined here carries the "rclone-"
# prefix: storage.cfg shares one property namespace between all plugins and
# a duplicate name makes PVE::Storage fail to load on the whole node.
package PVE::Storage::Custom::RcloneBackup::Schema;

use v5.36;

my $duration = '(?:0|\d+[smhdw])';
my $size = '\d+[KMGT]?';

my $properties = {
    'rclone-remote' => {
        title => 'Remote',
        description => "Name of the transport remote configured with 'pve-rclone-backup remote add'.",
        type => 'string',
        pattern => '[a-z][a-z0-9_-]{0,62}',
    },
    'rclone-path' => {
        title => 'Repository Path',
        description => 'Repository base path inside the remote.',
        type => 'string',
        pattern => '[A-Za-z0-9][A-Za-z0-9._-]*(?:/[A-Za-z0-9][A-Za-z0-9._-]*)*',
        maxLength => 128,
        default => 'pve-backups',
    },
    'rclone-encryption' => {
        title => 'Encryption',
        description => 'Client-side encryption of the repository (rclone crypt).',
        type => 'string',
        enum => ['crypt', 'none'],
        default => 'crypt',
    },
    'rclone-source' => {
        title => 'Source Name',
        description => 'Namespace of this Proxmox VE installation inside the repository.',
        type => 'string',
        pattern => '[a-z0-9][a-z0-9-]{0,31}',
    },
    'rclone-replicate-from' => {
        title => 'Replicate From',
        description => 'Local backup storages whose finished archives are replicated. Empty disables replication.',
        type => 'string',
        format => 'pve-storage-id-list',
    },
    'rclone-guests' => {
        title => 'Guests',
        description => 'Which guests are replicated: all, guests carrying one of rclone-tags, or rclone-vmids.',
        type => 'string',
        enum => ['all', 'tagged', 'listed'],
        default => 'all',
    },
    'rclone-tags' => {
        title => 'Tags',
        description => 'Guest tags selecting guests when rclone-guests is "tagged".',
        type => 'string',
        format => 'pve-tag-list',
        default => 'offsite',
    },
    'rclone-vmids' => {
        title => 'Guest IDs',
        description => 'Guests replicated when rclone-guests is "listed".',
        type => 'string',
        format => 'pve-vmid-list',
    },
    'rclone-exclude-vmids' => {
        title => 'Excluded Guest IDs',
        description => 'Guests never replicated.',
        type => 'string',
        format => 'pve-vmid-list',
    },
    'rclone-backfill' => {
        title => 'Backfill',
        description => 'Existing archives to replicate when replication is first enabled.',
        type => 'string',
        enum => ['none', 'latest', 'all'],
        default => 'latest',
    },
    'rclone-min-interval' => {
        title => 'Minimum Interval',
        description => 'Skip an archive if the guest already has an offsite backup newer than this (e.g. 7d).',
        type => 'string',
        pattern => $duration,
        default => '0',
    },
    'rclone-supersede' => {
        title => 'Supersede',
        description => 'Drop queued, not yet started uploads of older archives of the same guest.',
        type => 'boolean',
        default => 1,
    },
    'rclone-transfers' => {
        title => 'Concurrent Uploads',
        description => 'Number of archives uploaded concurrently.',
        type => 'integer',
        minimum => 1,
        maximum => 8,
        default => 2,
    },
    'rclone-bwlimit' => {
        title => 'Upload Bandwidth Limit',
        description => 'Upload bandwidth limit in rclone syntax, optionally a timetable such as "08:00,1M 23:00,off".',
        type => 'string',
        maxLength => 256,
    },
    'rclone-segment-size' => {
        title => 'Segment Size',
        description => 'Size of the segments archives are split into.',
        type => 'string',
        pattern => $size,
        default => '1G',
    },
    'rclone-verify-interval' => {
        title => 'Verify Interval',
        description => 'Interval of listing-based verification (presence and provider checksums).',
        type => 'string',
        pattern => $duration,
        default => '1d',
    },
    'rclone-verify-content' => {
        title => 'Content Verify Interval',
        description => 'Interval of rolling download-based verification, or "off".',
        type => 'string',
        pattern => "(?:off|$duration)",
        default => 'off',
    },
    'rclone-verify-budget' => {
        title => 'Content Verify Budget',
        description => 'Maximum bytes downloaded per day for content verification.',
        type => 'string',
        pattern => $size,
        default => '100G',
    },
    'rclone-min-age' => {
        title => 'Minimum Age',
        description => 'Retention never deletes offsite backups younger than this.',
        type => 'string',
        pattern => $duration,
        default => '7d',
    },
    'rclone-keep-min' => {
        title => 'Keep Minimum',
        description => 'Retention keeps at least this many complete, verified backups per guest.',
        type => 'integer',
        minimum => 0,
        default => 1,
    },
    'rclone-delete-grace' => {
        title => 'Delete Grace Period',
        description => 'Deleted offsite backups are kept as tombstones for this long and can be restored.',
        type => 'string',
        pattern => $duration,
        default => '7d',
    },
    'rclone-max-deletes' => {
        title => 'Maximum Deletions',
        description => 'Maximum number of backups a single retention run may delete.',
        type => 'integer',
        minimum => 0,
        default => 25,
    },
    'rclone-immutable' => {
        title => 'Immutable',
        description => 'Refuse every deletion of offsite backups.',
        type => 'boolean',
        default => 0,
    },
};

my $options = {
    'rclone-remote' => {},
    'rclone-path' => { fixed => 1, optional => 1 },
    'rclone-encryption' => { fixed => 1, optional => 1 },
    'rclone-source' => { fixed => 1 },
    (map { $_ => { optional => 1 } } grep {
        !m/^rclone-(?:remote|path|encryption|source)$/
    } keys %$properties),
    # Base properties shared with all storage plugins.
    content => { optional => 1 },
    nodes => { optional => 1 },
    disable => { optional => 1 },
    shared => { optional => 1 },
    'prune-backups' => { optional => 1 },
    'max-protected-backups' => { optional => 1 },
    bwlimit => { optional => 1 },
};

my $advanced = { map { $_ => 1 } qw(
    rclone-min-interval rclone-supersede rclone-transfers rclone-segment-size
    rclone-verify-interval rclone-verify-content rclone-verify-budget
    rclone-min-age rclone-keep-min rclone-delete-grace rclone-max-deletes
) };

sub properties { return { map { $_ => { $properties->{$_}->%* } } keys %$properties } }
sub options { return { map { $_ => { $options->{$_}->%* } } keys %$options } }
sub advanced_properties { return { %$advanced } }
sub hidden_properties { return {} }

1;
