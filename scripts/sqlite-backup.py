#!/usr/bin/env python3
"""Consistent SQLite online backup; offline-only restore without overwriting files."""
import argparse
import hashlib
import os
from pathlib import Path
import sqlite3
import sys

def verify(path: Path) -> None:
    with sqlite3.connect(path.as_uri() + '?mode=ro', uri=True) as connection:
        result = connection.execute('PRAGMA integrity_check').fetchall()
        if result != [('ok',)]:
            raise RuntimeError(f'integrity check failed: {result[:5]}')

def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['backup', 'verify', 'restore'])
    parser.add_argument('source', type=Path)
    parser.add_argument('destination', nargs='?', type=Path)
    parser.add_argument('--stopped', action='store_true', help='Confirm the control plane has stopped before restoring')
    args = parser.parse_args()
    source = args.source.resolve(strict=True)
    if args.action == 'verify':
        verify(source)
        print('SQLite integrity_check: ok')
        return
    if args.destination is None:
        parser.error('destination is required')
    destination = args.destination.resolve()
    if source == destination or destination.exists():
        parser.error('destination must be a new file different from source; existing databases are never overwritten')
    if args.action == 'restore':
        if not args.stopped:
            parser.error('stop all control-plane instances, then pass --stopped')
        if any(Path(str(destination) + suffix).exists() for suffix in ['-wal', '-shm']):
            parser.error('destination has SQLite sidecar files; use a new empty data directory')
        verify(source)
    destination.parent.mkdir(parents=True, exist_ok=True)
    # Reserve the destination exclusively to avoid overwriting another backup.
    descriptor = os.open(destination, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
    os.close(descriptor)
    with sqlite3.connect(source.as_uri() + '?mode=ro', uri=True) as incoming, sqlite3.connect(destination) as outgoing:
        incoming.backup(outgoing, pages=256)
    verify(destination)
    with destination.open('rb') as stream:
        checksum = hashlib.file_digest(stream, 'sha256').hexdigest()
    destination.with_suffix(destination.suffix + '.sha256').write_text(f'{checksum}  {destination.name}\n', encoding='utf-8')
    print(f'{args.action}: {destination}\nSHA256: {checksum}')

if __name__ == '__main__':
    try:
        main()
    except (OSError, sqlite3.Error, RuntimeError) as error:
        print(str(error), file=sys.stderr)
        sys.exit(1)
