# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Backup provider of the rclone-backup storage type.
#
# Offsite backups are bit-identical vzdump archives replicated by
# pve-rclone-backupd from local storages. This provider therefore refuses
# direct backups and serves archive metadata (guest and firewall
# configuration) from the daemon's catalogue, which works offline.
# Restores through the Proxmox VE GUI follow in a later release; until then
# the pve-rclone-backup CLI restores and fetches offsite backups.
package PVE::BackupProvider::Plugin::Rclone;

use v5.36;

use PVE::Storage::Custom::RcloneBackup::Client;

use base qw(PVE::BackupProvider::Plugin::Base);

my $NO_DIRECT_BACKUP = "direct backups to an rclone-backup storage are not supported:"
    . " back up to a local storage listed in 'rclone-replicate-from' and"
    . " pve-rclone-backupd replicates finished archives offsite\n";

my $NO_GUI_RESTORE = "restoring offsite backups from the Proxmox VE GUI is not supported yet:"
    . " use 'pve-rclone-backup restore <volume>' or 'pve-rclone-backup backup fetch <volume>'\n";

sub new($class, $storage_plugin, $scfg, $storeid, $log_function) {
    return bless {
        'storage-plugin' => $storage_plugin,
        scfg => $scfg,
        storeid => $storeid,
        'log-function' => $log_function,
    }, $class;
}

sub provider_name($self) { return 'pve-rclone-backup' }

# Backup API: refused, see $NO_DIRECT_BACKUP.

sub job_init($self, $start_time) { die $NO_DIRECT_BACKUP }
sub job_cleanup($self) { return }
sub backup_init($self, $vmid, $vmtype, $start_time) { die $NO_DIRECT_BACKUP }
sub backup_cleanup($self, $vmid, $vmtype, $success, $info) { return { stats => {} } }
sub backup_get_mechanism($self, $vmid, $vmtype) { die $NO_DIRECT_BACKUP }
sub backup_handle_log_file($self, $vmid, $filename) { return }
sub backup_vm_query_incremental($self, $vmid, $volumes) { return }
sub backup_vm($self, @) { die $NO_DIRECT_BACKUP }
sub backup_container_prepare($self, @) { die $NO_DIRECT_BACKUP }
sub backup_container($self, @) { die $NO_DIRECT_BACKUP }

# Restore API.

sub _config($self, $volname) {
    my $plugin = $self->{'storage-plugin'};
    my (undef, $name) = $plugin->parse_volname($volname);
    my $path = '/v1/storages/'
        . PVE::Storage::Custom::RcloneBackup::Client::uri_escape($self->{storeid})
        . '/backups/' . PVE::Storage::Custom::RcloneBackup::Client::uri_escape($name)
        . '/config';
    my $res = PVE::Storage::Custom::RcloneBackup::Client::request('GET', $path, timeout => 10);
    die "rclone-backup: unexpected configuration response from daemon\n" if ref($res) ne 'HASH';
    return $res;
}

sub archive_get_guest_config($self, $volname, @) {
    my $config = $self->_config($volname)->{config};
    die "rclone-backup: no guest configuration recorded for '$volname'\n"
        if !defined($config) || !length($config);
    return $config;
}

sub archive_get_firewall_config($self, $volname, @) {
    my $res = $self->_config($volname);
    die "rclone-backup: firewall configuration of '$volname' is unknown\n"
        if exists($res->{firewall_known}) && !$res->{firewall_known};
    my $fw = $res->{firewall};
    return (defined($fw) && length($fw)) ? $fw : undef;
}

sub restore_get_mechanism($self, $volname, @) { die $NO_GUI_RESTORE }
sub restore_vm_init($self, $volname, @) { die $NO_GUI_RESTORE }
sub restore_vm_cleanup($self, $volname, @) { return }
sub restore_vm_volume_init($self, $volname, @) { die $NO_GUI_RESTORE }
sub restore_vm_volume_cleanup($self, $volname, @) { return }
sub restore_container_init($self, $volname, @) { die $NO_GUI_RESTORE }
sub restore_container_cleanup($self, $volname, @) { return }

1;
