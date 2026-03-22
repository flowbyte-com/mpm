#!/bin/bash

# MPM Backup - Local backup
# Usage: ./gitlab-backup.sh [backup|status|list|restore]

WORKSPACE="$HOME/.openclaw/workspace"
BACKUP_DIR="$WORKSPACE/backups"
TIMESTAMP=$(date '+%Y%m%d-%H%M%S')

# What to backup
BACKUP_ITEMS=(
    "SOUL.md"
    "AGENTS.md"
    "USER.md"
    "TOOLS.md"
    "MEMORY.md"
    "IDENTITY.md"
    "HEARTBEAT.md"
    "MPM/"
)

# Create backup archive
do_backup() {
    mkdir -p "$BACKUP_DIR"
    
    local archive="$BACKUP_DIR/mpm-backup-${TIMESTAMP}.tar.gz"
    local items=()
    
    cd "$WORKSPACE"
    
    # Collect existing files
    for item in "${BACKUP_ITEMS[@]}"; do
        if [ -e "$item" ]; then
            items+=("$item")
        fi
    done
    
    if [ ${#items[@]} -eq 0 ]; then
        echo "❌ Nothing to backup"
        exit 1
    fi
    
    # Create archive (exclude MPM/backup to avoid recursion)
    tar --exclude='MPM/backup' -czf "$archive" "${items[@]}" 2>/dev/null
    
    if [ $? -eq 0 ]; then
        local size=$(du -h "$archive" | cut -f1)
        echo "✅ Backup created: $archive ($size)"
        echo ""
        echo "Contents:"
        for item in "${items[@]}"; do
            echo "  ✅ $item"
        done
        
        # Cleanup old backups (keep last 10)
        local count=$(ls -1 "$BACKUP_DIR"/mpm-backup-*.tar.gz 2>/dev/null | wc -l)
        if [ "$count" -gt 10 ]; then
            ls -1t "$BACKUP_DIR"/mpm-backup-*.tar.gz | tail -n +11 | xargs rm -f
            echo ""
            echo "🧹 Cleaned old backups (kept last 10)"
        fi
    else
        echo "❌ Backup failed"
        exit 1
    fi
}

# Show backup status
show_status() {
    echo "📊 MPM Backup Status"
    echo "==================="
    echo ""
    
    if [ -d "$BACKUP_DIR" ]; then
        local count=$(ls -1 "$BACKUP_DIR"/mpm-backup-*.tar.gz 2>/dev/null | wc -l)
        local total=$(du -sh "$BACKUP_DIR" 2>/dev/null | cut -f1)
        echo "  Backups: $count ($total)"
        
        if [ "$count" -gt 0 ]; then
            local latest=$(ls -1t "$BACKUP_DIR"/mpm-backup-*.tar.gz | head -1)
            local latest_name=$(basename "$latest")
            local latest_size=$(du -h "$latest" | cut -f1)
            echo "  Latest:  $latest_name ($latest_size)"
        fi
    else
        echo "  No backups yet"
    fi
    
    echo ""
    echo "📁 Backup Items"
    echo "---------------"
    cd "$WORKSPACE"
    for item in "${BACKUP_ITEMS[@]}"; do
        if [ -e "$item" ]; then
            echo "  ✅ $item"
        else
            echo "  ❌ $item (missing)"
        fi
    done
    
    echo ""
    echo "Commands:"
    echo "  mpm backup           # Create backup"
    echo "  mpm backup status    # This view"
    echo "  mpm backup restore   # Restore latest"
    echo "  mpm backup list      # List all backups"
}

# List all backups
list_backups() {
    echo "📦 Available Backups"
    echo "==================="
    echo ""
    
    if [ ! -d "$BACKUP_DIR" ]; then
        echo "  No backups yet. Run: mpm backup"
        return
    fi
    
    ls -1t "$BACKUP_DIR"/mpm-backup-*.tar.gz 2>/dev/null | while read -r f; do
        local name=$(basename "$f")
        local size=$(du -h "$f" | cut -f1)
        echo "  $name ($size)"
    done
    
    local count=$(ls -1 "$BACKUP_DIR"/mpm-backup-*.tar.gz 2>/dev/null | wc -l)
    if [ "$count" -eq 0 ]; then
        echo "  No backups yet. Run: mpm backup"
    fi
}

# Restore from latest backup
restore_backup() {
    local target="${1:-}"
    
    # Use specific backup or latest
    if [ -n "$target" ] && [ -f "$target" ]; then
        local archive="$target"
    elif [ -n "$target" ] && [ -f "$BACKUP_DIR/$target" ]; then
        local archive="$BACKUP_DIR/$target"
    else
        local archive=$(ls -1t "$BACKUP_DIR"/mpm-backup-*.tar.gz 2>/dev/null | head -1)
    fi
    
    if [ -z "$archive" ] || [ ! -f "$archive" ]; then
        echo "❌ No backup found to restore"
        echo "   Run: mpm backup list"
        exit 1
    fi
    
    echo "♻️  Restoring from: $(basename "$archive")"
    echo ""
    
    cd "$WORKSPACE"
    tar -xzf "$archive"
    
    if [ $? -eq 0 ]; then
        echo "✅ Restore complete"
        echo ""
        echo "Restored files:"
        tar -tzf "$archive" | head -20
    else
        echo "❌ Restore failed"
        exit 1
    fi
}

# Main
case "${1:-backup}" in
    backup|commit)
        do_backup
        ;;
    status)
        show_status
        ;;
    list|ls)
        list_backups
        ;;
    restore)
        restore_backup "$2"
        ;;
    *)
        echo "Usage: $0 [backup|status|list|restore] [options]"
        echo ""
        echo "Commands:"
        echo "  backup          # Create backup archive"
        echo "  status          # Show backup status"
        echo "  list            # List all backups"
        echo "  restore [file]  # Restore from backup (latest if no file)"
        exit 1
        ;;
esac
