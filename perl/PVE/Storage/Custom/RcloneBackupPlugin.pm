# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Proxmox VE storage plugin exposing pve-rclone-backup offsite repositories.
#
# This is a thin adapter: every operation is answered by pve-rclone-backupd
# over its local API. Listing and status are served from the daemon's
# catalogue and never touch the network, so pvestatd cannot block on a slow
# or unreachable cloud provider.
#
# Backups are not written to this storage directly. vzdump writes to a local
# storage and the daemon replicates finished archives (see
# 'rclone-replicate-from'). The storage declares the backup-provider feature
# so offsite backups integrate with PVE's backup listing, configuration
# extraction and (later) restore code paths.
package PVE::Storage::Custom::RcloneBackupPlugin;

use v5.36;

use PVE::Storage::Plugin;

use PVE::BackupProvider::Plugin::Rclone;
use PVE::Storage::Custom::RcloneBackup::Client;
use PVE::Storage::Custom::RcloneBackup::Schema;

use base qw(PVE::Storage::Plugin);

# Storage plugin API range this plugin is tested against.
use constant APIVER_MIN => 12; # Proxmox VE 9.0
use constant APIVER_MAX => 15; # Proxmox VE 9.2

# Matches the original vzdump archive name, optionally with a collision
# suffix (".N") before the extension.
our $VOLNAME_RE = qr!^backup/(vzdump-(qemu|lxc|openvz)-([1-9][0-9]{2,8})-
    [0-9]{4}_[0-9]{2}_[0-9]{2}-[0-9]{2}_[0-9]{2}_[0-9]{2}(?:\.[1-9][0-9]{0,3})?
    \.(tgz|(?:vma|tar)(?:\.(?:gz|lzo|zst|bz2))?))$!x;

my $VIRTUAL_DIR = '/run/pve-rclone-backup/virtual';

sub api {
    my $apiver = eval { PVE::Storage::APIVER() } // APIVER_MAX;
    return $apiver if $apiver >= APIVER_MIN && $apiver <= APIVER_MAX;
    # Outside the tested range: report our newest version. PVE accepts it
    # while it is within its APIAGE window and refuses it otherwise.
    return APIVER_MAX;
}

sub type { return 'rclone-backup' }

sub plugindata {
    return {
        content => [{ backup => 1, none => 1 }, { backup => 1 }],
        features => { 'backup-provider' => 1 },
        # Secrets are managed by pve-rclone-backup, never via storage.cfg.
        'sensitive-properties' => {},
        # Understood by the upcoming schema-driven storage GUI; ignored by
        # older Proxmox VE releases.
        'advanced-properties' => PVE::Storage::Custom::RcloneBackup::Schema::advanced_properties(),
        'hidden-properties' => PVE::Storage::Custom::RcloneBackup::Schema::hidden_properties(),
    };
}

sub properties { return PVE::Storage::Custom::RcloneBackup::Schema::properties() }
sub options { return PVE::Storage::Custom::RcloneBackup::Schema::options() }

sub check_config($class, $sectionId, $config, $create, $skipSchemaCheck) {
    my $opts = $class->SUPER::check_config($sectionId, $config, $create, $skipSchemaCheck);

    # The repository is reachable from every node.
    $opts->{shared} = 1 if $create || exists $opts->{shared};

    my $guests = $opts->{'rclone-guests'} // 'all';
    die "rclone-guests 'listed' requires rclone-vmids\n"
        if $guests eq 'listed' && $create && !$opts->{'rclone-vmids'};

    return $opts;
}

# Storage configuration hooks run under the storage.cfg lock: they only ask
# the daemon to validate (fast, no network) and never change remote state.

sub on_add_hook($class, $storeid, $scfg, %sensitive) {
    _request('POST', _storage_path($storeid) . '/validate',
        body => { config => $scfg }, timeout => 10);
    return undef;
}

sub on_update_hook_full($class, $storeid, $scfg, $update, $delete, $sensitive) {
    _request('POST', _storage_path($storeid) . '/validate',
        body => { config => $scfg, update => $update, delete => $delete }, timeout => 10);
    return undef;
}

sub on_update_hook($class, $storeid, $scfg, %param) {
    return $class->on_update_hook_full($storeid, $scfg, \%param, [], {});
}

sub on_delete_hook($class, $storeid, $scfg) {
    # Offsite data and keys are deliberately left untouched.
    return undef;
}

sub _storage_path($storeid) {
    return '/v1/storages/' . PVE::Storage::Custom::RcloneBackup::Client::uri_escape($storeid);
}

sub _backup_path($storeid, $volname) {
    my (undef, $name) = __PACKAGE__->parse_volname($volname);
    return _storage_path($storeid) . '/backups/'
        . PVE::Storage::Custom::RcloneBackup::Client::uri_escape($name);
}

sub _request { return PVE::Storage::Custom::RcloneBackup::Client::request(@_) }

sub parse_volname($class, $volname) {
    if ($volname =~ $VOLNAME_RE) {
        my ($name, $vmtype, $vmid, $format) = ($1, $2, $3, $4);
        return ('backup', $name, $vmid, undef, undef, undef, $format);
    }
    die "unable to parse rclone-backup volume name '$volname'\n";
}

# There is no local file behind a volume. Return a path in a directory that
# never holds real files: PVE's delete API removes '.log'/'.notes' files next
# to whatever path() returns.
sub path($class, $scfg, $volname, $storeid, $snapname = undef) {
    die "volume snapshots are not supported on rclone-backup storage\n" if $snapname;
    my (undef, $name, $vmid) = $class->parse_volname($volname);
    $storeid =~ m/^([a-z][a-z0-9\-_.]*[a-z0-9])$/i
        or die "invalid storage ID '$storeid'\n";
    return ("$VIRTUAL_DIR/$1/$name", $vmid, 'backup');
}

sub status($class, $storeid, $scfg, $cache) {
    my $res = eval { _request('GET', _storage_path($storeid) . '/status', timeout => 2) };
    # Inactive rather than an error: pvestatd polls every few seconds.
    return (0, 0, 0, 0) if $@ || ref($res) ne 'HASH';
    my @v = map { (($res->{$_} // 0) =~ m/^(\d+)$/) ? int($1) : 0 } qw(total avail used);
    return (@v, $res->{active} ? 1 : 0);
}

# PVE activates a storage being added before it writes storage.cfg, so the
# daemon does not serve it yet: its configuration is validated instead
# (fast, no network). Returns the status, or undef for such a new storage.
sub _status_or_validate($storeid, $scfg) {
    my $res = eval { _request('GET', _storage_path($storeid) . '/status', timeout => 3) };
    return $res if !$@;
    my $err = $@;
    die $err if ($PVE::Storage::Custom::RcloneBackup::Client::LAST_ERROR_CODE // '') ne 'not_found';
    _request('POST', _storage_path($storeid) . '/validate', body => { config => $scfg }, timeout => 10);
    return undef;
}

sub activate_storage($class, $storeid, $scfg, $cache = undef, $hints = undef) {
    # Succeeds while the daemon serves this storage, even if the remote is
    # offline: the catalogue stays browsable.
    _status_or_validate($storeid, $scfg);
    return 1;
}

sub deactivate_storage($class, $storeid, $scfg, $cache = undef) { return 1 }

sub check_connection($class, $storeid, $scfg) {
    my $res = eval { _status_or_validate($storeid, $scfg) };
    return 0 if $@;
    return 1 if !defined($res);
    return (ref($res) eq 'HASH' && $res->{active}) ? 1 : 0;
}

sub activate_volume($class, $storeid, $scfg, $volname, $snapname = undef, $cache = undef, $hints = undef) {
    die "volume snapshots are not supported on rclone-backup storage\n" if $snapname;
    return 1;
}

sub deactivate_volume($class, $storeid, $scfg, $volname, $snapname = undef, $cache = undef) {
    die "volume snapshots are not supported on rclone-backup storage\n" if $snapname;
    return 1;
}

sub list_images($class, $storeid, $scfg, $vmid = undef, $vollist = undef, $cache = undef) {
    return [];
}

sub list_volumes($class, $storeid, $scfg, $vmid, $content_types) {
    return [] if !grep { $_ eq 'backup' } @$content_types;

    my $res = _request('GET', _storage_path($storeid) . '/backups',
        query => { vmid => $vmid }, timeout => 10) // [];
    die "rclone-backup: unexpected backup list from daemon\n" if ref($res) ne 'ARRAY';

    my $list = [];
    for my $b (@$res) {
        my ($volname) = ($b->{volname} // '') =~ m/^(backup\/\S+)$/ or next;
        my (undef, undef, $owner, undef, undef, undef, $format) =
            eval { $class->parse_volname($volname) } or next;
        my $subtype = $b->{vmtype} // '';
        $subtype = '' if $subtype ne 'qemu' && $subtype ne 'lxc';
        my $entry = {
            volid => "$storeid:$volname",
            vmid => int($owner),
            content => 'backup',
            format => $format,
            size => _int($b->{size}),
            ctime => _int($b->{ctime}),
            protected => $b->{protected} ? 1 : 0,
        };
        $entry->{subtype} = $subtype if $subtype;
        $entry->{notes} = $b->{notes} if defined $b->{notes} && length $b->{notes};
        push @$list, $entry;
    }
    return $list;
}

sub _int($v) { return (defined($v) && $v =~ m/^(\d+)$/) ? int($1) : 0 }

sub volume_size_info($class, $scfg, $storeid, $volname, $timeout = undef) {
    my $b = _request('GET', _backup_path($storeid, $volname), timeout => 10);
    my (undef, undef, undef, undef, undef, undef, $format) = $class->parse_volname($volname);
    my $size = _int($b->{size});
    return wantarray ? ($size, $format, $size, undef, _int($b->{ctime})) : $size;
}

sub get_volume_attribute($class, $scfg, $storeid, $volname, $attribute) {
    return undef if $attribute ne 'notes' && $attribute ne 'protected';
    my $b = _request('GET', _backup_path($storeid, $volname), timeout => 10);
    return ($b->{protected} ? 1 : 0) if $attribute eq 'protected';
    return $b->{notes};
}

sub update_volume_attribute($class, $scfg, $storeid, $volname, $attribute, $value) {
    die "attribute '$attribute' is not supported on rclone-backup storage\n"
        if $attribute ne 'notes' && $attribute ne 'protected';
    $value = $value ? JSON::true : JSON::false if $attribute eq 'protected';
    _request('PATCH', _backup_path($storeid, $volname),
        body => { $attribute => $value }, timeout => 30);
    return undef;
}

# Deleting an offsite backup only tombstones it; the daemon removes it after
# the configured grace period and refuses protected or immutable backups.
sub free_image($class, $storeid, $scfg, $volname, $isBase = undef, $format = undef) {
    _request('DELETE', _backup_path($storeid, $volname), timeout => 30);
    return undef;
}

sub prune_backups($class, $scfg, $storeid, $keep, $vmid, $type, $dryrun, $logfunc) {
    $logfunc //= sub { print "$_[1]\n" };
    my $body = { keep => $keep, type => $type, dry_run => $dryrun ? JSON::true : JSON::false };
    # The API passes the VMID as a string; the daemon expects a number.
    $body->{vmid} = _int($vmid) if defined($vmid);
    my $res = _request('POST', _storage_path($storeid) . '/prune', timeout => 30, body => $body) // [];
    die "rclone-backup: unexpected prune result from daemon\n" if ref($res) ne 'ARRAY';

    my $list = [];
    for my $e (@$res) {
        my $entry = {
            volid => $e->{volid},
            ctime => _int($e->{ctime}),
            type => $e->{type},
            mark => $e->{mark},
        };
        $entry->{vmid} = _int($e->{vmid}) if defined $e->{vmid};
        $logfunc->('info', "tombstoning offsite backup '$e->{volid}'")
            if !$dryrun && ($e->{mark} // '') eq 'remove';
        push @$list, $entry;
    }
    return $list;
}

sub get_identity($class, $scfg, $storeid) {
    my $res = _request('GET', _storage_path($storeid), timeout => 3);
    my ($uuid) = ($res->{repo_uuid} // '') =~ m/^([0-9a-f-]{36})$/
        or die "rclone-backup: repository of '$storeid' is not initialized\n";
    return $uuid;
}

sub volume_has_feature { return 0 }

sub alloc_image { die "cannot allocate images on rclone-backup storage\n" }
sub clone_image { die "cannot clone images on rclone-backup storage\n" }
sub create_base { die "cannot create base images on rclone-backup storage\n" }
sub rename_volume { die "cannot rename volumes on rclone-backup storage\n" }
sub volume_resize { die "cannot resize volumes on rclone-backup storage\n" }
sub volume_snapshot { die "volume snapshots are not supported on rclone-backup storage\n" }
sub volume_snapshot_rollback { die "volume snapshots are not supported on rclone-backup storage\n" }
sub volume_snapshot_delete { die "volume snapshots are not supported on rclone-backup storage\n" }

sub new_backup_provider($class, $scfg, $storeid, $log_function) {
    return PVE::BackupProvider::Plugin::Rclone->new($class, $scfg, $storeid, $log_function);
}

1;
