# Clusters

pve-rclone-backup works in a Proxmox VE cluster. Full cluster support, with automatic takeover of a
failed node's uploads, is planned but not built yet. Today:

- **Install the package on every node** whose backups should go offsite.
- **Set up once.** Remotes, keys, recovery kit confirmations and the daemon settings are kept in
  `/etc/pve`, which Proxmox VE shares between all nodes. Storages live in `/etc/pve/storage.cfg`.
  Run `remote add` and `storage init` on any one node.
- **Each node uploads its own backups.** Every node runs its own daemon, with its own queue and
  catalogue.

## Who uploads what

| Source storage | Uploaded by |
|---|---|
| Local to each node, such as `local` | The node that wrote the archive. |
| Shared by the nodes, such as an NFS share | The node the guest currently runs on, or the node that saw the archive appear. For a guest that no longer exists, the cluster's first online node (by name). |

If two nodes ever upload the same archive, the second finds it already offsite and stops.

Limit a storage to some nodes with its `nodes` property; other nodes neither replicate to it nor
list it.

## Cluster-wide tasks

Scheduled retention, the deletion of backups whose grace period ended, and the planning of rolling
content verification run on one node: the online node whose name sorts first.

## Catalogues

Each node's catalogue is rebuilt from the cloud regularly, at least every 6 hours. A backup uploaded
by one node therefore shows on the other nodes after a delay. To see it at once on another node:

```sh
pve-rclone-backup storage resync offsite
```

## Restores

Restores and fetches run on the node where you start them, and stage archives on that node.

## If a node fails

The node's queued uploads wait until it is back. Archives on a shared storage are picked up by the
node a guest moves to. Uploads of archives on the failed node's local storage cannot continue
elsewhere.

The offsite backups themselves do not depend on any node: every node, or a new installation with the
recovery kit, can list and restore them.
