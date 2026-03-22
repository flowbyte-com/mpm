#!/bin/bash

# MPM Backup - Backup core agent files to MPM/backup/
# Usage: ./backup.sh [--backup|--restore|--list|--status]

WORKSPACE="$HOME/.openclaw/workspace"
BACKUP_DIR="$WORKSPACE/MPM/backup"
TIMESTAMP=$(date '+%Y-%m-%d_%H-%M-%S')

# Core files to backup
CORE_FILES=(
    "SOUL.md"
    "AGENTS.md"
    "USER.md"
    "TOOLS.md"
    "IDENTITY.md"
    "MEMORY.md"
    "HEARTBEAT.md"
    "WEBSITE.md"
)

# Core directories to backup
CORE_DIRS=(
    "personas"
    "modes"
    "memory"
    "memory-topics"
    "projects"
    "skills"
)

ensure_backup_dir() {
    mkdir -p "$BACKUP_DIR"
}

show_status() {
    echo "📦 MPM Backup Status"
    echo "==================="
    echo ""
    
    if [ -d "$BACKUP_DIR" ]; then
        backup_count=$(ls -d "$BACKUP_DIR"/backup-* 2>/dev/null | wc -l)
        latest=$(ls -t "$BACKUP_DIR" 2>/dev/null | head -1)
        
        echo "Backup dir: $BACKUP_DIR"
        echo "Backups: $backup_count"
        
        if [ -n "$latest" ]; then
            echo "Latest: $latest"
            ls -lh "$BACKUP_DIR/$latest" 2>/dev/null | head -5
        fi
    else
        echo "❌ No backups found"
        echo "   Run: ./backup.sh --backup"
    fi
    echo ""
}

list_backups() {
    echo "📦 Available Backups"
    echo "==================="
    echo ""
    
    if [ -d "$BACKUP_DIR" ]; then
        ls -lht "$BACKUP_DIR"/backup-* 2>/dev/null | head -20
    else
        echo "(no backups)"
    fi
    echo ""
}

do_backup() {
    local backup_name="backup-$TIMESTAMP"
    local backup_path="$BACKUP_DIR/$backup_name"
    
    echo "📦 Creating backup: $backup_name"
    echo ""
    
    ensure_backup_dir
    
    mkdir -p "$backup_path/files"
    mkdir -p "$backup_path/dirs"
    
    # Backup core files (copy as-is)
    echo "Copying core files..."
    for file in "${CORE_FILES[@]}"; do
        if [ -f "$WORKSPACE/$file" ]; then
            cp "$WORKSPACE/$file" "$backup_path/files/"
            echo "  ✅ $file"
        else
            echo "  ⚠️  $file (not found)"
        fi
    done
    
    # Backup core directories (copy as-is)
    echo ""
    echo "Copying core directories..."
    for dir in "${CORE_DIRS[@]}"; do
        if [ -d "$WORKSPACE/$dir" ]; then
            cp -r "$WORKSPACE/$dir" "$backup_path/dirs/"
            echo "  ✅ $dir/"
        else
            echo "  ⚠️  $dir/ (not found)"
        fi
    done
    
    # Create manifest
    cat > "$backup_path/manifest.txt" << MANIFEST
Backup: $backup_name
Created: $(date '+%Y-%m-%d %H:%M:%S')
Workspace: $WORKSPACE

Files:
$(ls "$backup_path/files/" 2>/dev/null)

Directories:
$(ls "$backup_path/dirs/" 2>/dev/null)
MANIFEST
    
    echo ""
    echo "✅ Backup complete: $backup_path"
    echo ""
    echo "To restore:"
    echo "  ./backup.sh --restore $backup_name"
}

do_restore() {
    local backup_name="$1"
    local backup_path="$BACKUP_DIR/$backup_name"
    
    if [ -z "$backup_name" ]; then
        echo "Usage: $0 --restore <backup-name>"
        echo "Run '$0 --list' to see available backups"
        exit 1
    fi
    
    if [ ! -d "$backup_path" ]; then
        echo "❌ Backup not found: $backup_name"
        exit 1
    fi
    
    echo "📦 Restoring backup: $backup_name"
    echo ""
    echo "⚠️  This will overwrite files in workspace!"
    echo "    Continue? (y/n)"
    read -r confirm
    
    if [ "$confirm" != "y" ]; then
        echo "Cancelled"
        exit 0
    fi
    
    # Restore files
    if [ -d "$backup_path/files" ]; then
        echo "Restoring files..."
        cp "$backup_path/files"/* "$WORKSPACE/"
        echo "  ✅ Files restored"
    fi
    
    # Restore directories
    if [ -d "$backup_path/dirs" ]; then
        echo "Restoring directories..."
        for dir in "$backup_path/dirs"/*; do
            dirname=$(basename "$dir")
            rm -rf "$WORKSPACE/$dirname"
            cp -r "$dir" "$WORKSPACE/"
            echo "  ✅ $dirname/"
        done
    fi
    
    echo ""
    echo "✅ Restore complete!"
    echo "   Files restored to: $WORKSPACE"
}

# Local backup info
show_local_backup_info() {
    echo "📦 Local Backup"
    echo "  mpm backup - Create backup to $WORKSPACE/backups/"
}

# Main handler
case "${1:---status}" in
    --backup|backup)
        do_backup
        ;;
    --restore|restore)
        do_restore "$2"
        ;;
    --list|list)
        list_backups
        ;;
    --status|status)
        show_status
        ;;
    --info)
        show_local_backup_info
        ;;
    --help|help|-h)
        cat << 'EOF'
📦 MPM Backup - Core agent file protection

USAGE:
    ./backup.sh [--backup|--restore|--list|--status|--info]

COMMANDS:
    --backup    Create new backup of core files + dirs
    --restore   Restore from a backup (interactive)
    --list      List available backups
    --status    Show backup status
    --info      Show backup info

CORE FILES BACKED UP:
    SOUL.md, AGENTS.md, USER.md, TOOLS.md
    IDENTITY.md, MEMORY.md, HEARTBEAT.md, WEBSITE.md

CORE DIRS BACKED UP:
    personas/, modes/, memory/, memory-topics/
    projects/, skills/

BACKUP LOCATION:
    MPM/backup/backup-YYYY-MM-DD_HH-MM-SS/

BACKUP LOCATION:
    $WORKSPACE/backups/mpm-backup-YYYYMMDD-HHMMSS.tar.gz
EOF
        ;;
    *)
        echo "Usage: $0 [--backup|--restore|--list|--status]"
        echo "Run './backup.sh --help' for details"
        exit 1
        ;;
esac
