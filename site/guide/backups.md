# Backups

A backup job runs `pg_dumpall` (or `mysqldump`, or `mongodump`) **inside the database
container** and streams the result out:

```
docker exec pg_dumpall  →  gzip  →  age  →  S3 multipart upload
```

Nothing is buffered to disk at either end and memory stays flat, so a 100 GB database backs
up from a box whose disk is already full of that database.

## Encryption, and why restore is a CLI command

By default a snapshot is encrypted to an **age public key**. Daffa holds only the public
key, which means **Daffa cannot read its own backups** — and neither can anyone who steals
the bucket, or the server, or both. An attacker who compromises the box does not thereby
inherit every snapshot you have ever taken, including the ones from before they arrived.

Keys are managed under **Settings → Certificates**. Generate one there and Daffa creates the
keypair in memory, hands you the private half as a one-time download, and stores only the
public half — the download screen will not let you past until you have the file. Or generate
your own and import just the public key:

```sh
age-keygen -o key.txt      # prints the public key; store key.txt safely OFF this box
```

Backup jobs then *select* named keys. Pick two — a personal key and a break-glass key held
somewhere independent — and every snapshot is encrypted to both; any one private key
restores.

That is also why **restore is a CLI command and not a button**. Restoring needs the private
key; if the web UI asked you to paste it in, the key would reach the server and the whole
guarantee would evaporate. The console shows you the exact command, and the CLI does the
decryption locally:

```sh
daffa restore --server https://ops.example.com \
  --job <job-id> --snapshot <key> --user you --identity ~/key.txt
```

Daffa streams the *ciphertext* down to your machine, your machine decrypts it, and the
plaintext is streamed back into the database container. It asks you to type the job name
before it overwrites anything, and the restore is audited at both ends.

::: tip Unencrypted mode
You can turn encryption **off** (`encryption: none`), which stores a plain gzip dump;
restore then needs no key. The trade is exactly what it sounds like — anyone who can read
the bucket can read your database — and the UI labels those jobs as unencrypted.
:::

## Volume backups

For file-shaped data that has no dump tool — Forgejo repositories, uploads, provisioning
state — pick the **volume** engine and name a Docker volume instead of a container. Daffa
mounts it read-only in a throwaway helper and streams the daemon's own tar through the same
`gzip → age → S3` pipeline. No user container is touched.

::: warning Not for live databases
A file-level snapshot of a running database volume is torn and may not restore. Use a
database engine for databases. If a consumer must be quiet for a consistent copy, list it
under **Stop during snapshot** — Daffa stops it for the duration and restarts it after, even
if the snapshot fails.
:::

### Excluding paths

Volume jobs have an **Exclude paths** field: one path per line, relative to the volume root,
dropped from every snapshot. Use it for regenerable junk that need not be backed up —
caches, logs, search indexes, session temp files — to keep snapshots small and restores
fast.

```
cache
tmp/sessions
var/log
```

A pattern matches a file exactly, or a directory and its **entire subtree** — `cache` drops
`cache/` and everything under it. There are no wildcards, and matching is on path boundaries,
so `logs` does not touch a sibling file named `logs.txt`. Paths are relative to the volume
root: write `cache`, not `/data/cache`, and a path that tries to leave the volume (an
absolute path, or one with `..`) is refused when you save the job.

Deleting a backup job stops future backups. It never deletes snapshots that already exist —
Daffa does not touch your bucket's contents. Use your storage provider's lifecycle rules for
retention.
