#!/bin/bash
set -e
BACKUP_DIR=/root/briefing-v3/data/backups
mkdir -p "$BACKUP_DIR"
DATE=$(date -u +%Y-%m-%d)
cp /root/briefing-v3/data/briefing.db "$BACKUP_DIR/briefing-$DATE.db"
find "$BACKUP_DIR" -name 'briefing-*.db' -mtime +30 -delete
echo "backup done: $BACKUP_DIR/briefing-$DATE.db"
