#!/bin/bash

# Persona TUI - Terminal UI for persona management
# Requires: whiptail or dialog
# Usage: ./persona-tui.sh

# Detect correct workspace path
if [ -d "$HOME/.openclaw/workspace" ]; then
    WORKSPACE="$HOME/.openclaw/workspace"
elif [ -d "$HOME/.picoclaw/workspace" ]; then
    WORKSPACE="$HOME/.picoclaw/workspace"
else
    WORKSPACE="$HOME/.openclaw/workspace"
fi

PERSONAS_DIR="$WORKSPACE/MPM/persona"
LOADED_DIR="$PERSONAS_DIR/loaded"
DB_FILE="$PERSONAS_DIR/personas.db"
SCRIPT_DIR="$(dirname "$(readlink -f "$0")")"

# Check for whiptail/dialog
if command -v whiptail &> /dev/null; then
    DIALOG="whiptail"
elif command -v dialog &> /dev/null; then
    DIALOG="dialog"
else
    echo "❌ Requires whiptail or dialog"
    echo "   Install: sudo apt install whiptail"
    echo ""
    echo "Fallback: mpm persona list"
    exit 1
fi

# Ensure loaded directory exists
mkdir -p "$LOADED_DIR"

# Initialize database if needed
init_db() {
    sqlite3 "$DB_FILE" <<'SCHEMA' 2>/dev/null
CREATE TABLE IF NOT EXISTS personas (
    name TEXT PRIMARY KEY,
    identity_name TEXT,
    identity_title TEXT,
    identity_vibe TEXT,
    identity_emoji TEXT,
    voice_tone TEXT,
    behavioral_rules TEXT,
    context TEXT,
    activation TEXT,
    loaded_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS persona_files (
    name TEXT PRIMARY KEY,
    file_path TEXT,
    file_hash TEXT,
    last_synced TIMESTAMP,
    status TEXT
);
SCHEMA
}

# Build persona list for menu
build_menu() {
    local menu_items=()
    
    shopt -s nullglob
    for file in "$PERSONAS_DIR"/*.persona; do
        name=$(basename "$file" .persona)
        title=$(grep -E '^\- \*\*Name:\*\*' "$file" 2>/dev/null | head -1 | sed 's/^- \*\*Name:\*\* //')
        vibe=$(grep -E '^\- \*\*Vibe:\*\*' "$file" 2>/dev/null | head -1 | sed 's/^- \*\*Vibe:\*\* //')
        menu_items+=("$name" "$title - $vibe")
    done
    shopt -u nullglob
    
    # Add menu options
    menu_items+=("sync" "Sync all personas to DB")
    menu_items+=("list" "List loaded personas")
    menu_items+=("create" "Create new persona")
    menu_items+=("quickhelp" "Quick Links Help")
    menu_items+=("quit" "Exit to shell")
    
    printf '%s\n' "${menu_items[@]}"
}

# Show Quick Links Help
show_quickhelp() {
    local msg="
╔════════════════════════════════════════════════════════════╗
║                    ⚡ MPM QUICK LINKS                       ║
╠════════════════════════════════════════════════════════════╣
║  Persona Commands (~p)                                     ║
║    ~p              → Open interactive persona selector   ║
║    ~p.<name>       → Direct activate persona             ║
║    ~p.clear        → Clear persona (base)                 ║
║                                                            ║
║  Mode Commands (~m)                                         ║
║    ~m              → Open interactive mode selector        ║
║    ~m.<name>       → Load mode (clear others)              ║
║    ~m+<name>       → Add mode to stack                    ║
║    ~m-<name>       → Remove mode from stack                ║
║    ~m.clr          → Clear all stacked modes               ║
║                                                            ║
║  System Commands                                             ║
║    mpm status      → Show system heartbeat                 ║
║    mpm watch       → Launch file watcher                   ║
║    mpm help        → Full command reference                ║
╚════════════════════════════════════════════════════════════╝

Use ~p and ~m opcodes for fastest access!"

    $DIALOG --title "📚 Quick Links Help" --msgbox "$msg" 25 70
}

# Main menu loop
main_menu() {
    while true; do
        init_db
        
        # Build menu options
        local menu_args=()
        while IFS= read -r line; do
            menu_args+=("$line")
        done <<< "$(build_menu)"
        
        # Show menu
        local choice=$($DIALOG --title "🎭 Persona Manager v3.2" \
            --menu "Select action (or use ~p in terminal):" 20 80 15 \
            "${menu_args[@]}" 3>&1 1>&2 2>&3)
        
        local exit_status=$?
        if [ $exit_status != 0 ]; then
            clear
            echo "👋 Goodbye! Try 'mpm help' for command reference."
            exit 0
        fi
        
        case "$choice" in
            quit)
                clear
                echo "👋 Goodbye! Quick links: ~p | ~m | mpm status"
                exit 0
                ;;
            sync)
                "$SCRIPT_DIR/persona-compile.sh" sync
                $DIALOG --msgbox "✅ Sync complete!\n\nPersonas updated in database." 10 60
                ;;
            list)
                local output=$(sqlite3 -header -column "$DB_FILE" "SELECT name, identity_name, updated_at FROM personas;" 2>/dev/null)
                if [ -z "$output" ]; then
                    $DIALOG --msgbox "No personas loaded in DB.\n\nRun 'Sync' to load them." 10 60
                else
                    $DIALOG --title "📋 Loaded Personas" --msgbox "$output" 20 70
                fi
                ;;
            create)
                local name=$($DIALOG --inputbox "Enter persona name (alphanumeric, _- only):" 10 60 3>&1 1>&2 2>&3)
                if [ -n "$name" ]; then
                    # Validate
                    if [[ ! "$name" =~ ^[a-zA-Z0-9_-]+$ ]]; then
                        $DIALOG --msgbox "❌ Invalid name. Use only letters, numbers, underscore, hyphen." 10 60
                        continue
                    fi
                    
                    local file="$PERSONAS_DIR/${name}.persona"
                    if [ -f "$file" ]; then
                        $DIALOG --msgbox "❌ Persona already exists: $name" 10 60
                    else
                        cat > "$file" <<EOF
# Persona: ${name^}

## Identity Override
- **Name:** [Override name]
- **Title:** [Override title]
- **Vibe:** [Override personality]
- **Emoji:** [Override emoji]

## Voice & Tone
[Description]

## Behavioral Rules
- [Rule 1]
- [Rule 2]

## Context
[When to use]

## Activation
[How to activate]
EOF
                        $DIALOG --msgbox "✅ Created: $file\n\nEdit with:\nnano $file" 12 70
                        "$SCRIPT_DIR/persona-compile.sh" sync
                    fi
                fi
                ;;
            quickhelp)
                show_quickhelp
                ;;
            *)
                # It's a persona name - show submenu
                persona_submenu "$choice"
                ;;
        esac
    done
}

# Persona submenu (view, edit, activate, delete)
persona_submenu() {
    local name="$1"
    local file="$PERSONAS_DIR/${name}.persona"
    
    while true; do
        local choice=$($DIALOG --title "📄 $name" \
            --menu "Select action:" 20 70 10 \
            "view" "View persona content" \
            "edit" "Edit persona file" \
            "activate" "Activate this persona (~p.$name)" \
            "query" "Query DB fields" \
            "unload" "Unload from DB" \
            "delete" "⚠️ Delete persona file" \
            "back" "← Back to menu" \
            3>&1 1>&2 2>&3)
        
        local exit_status=$?
        if [ $exit_status != 0 ]; then
            return 0
        fi
        
        case "$choice" in
            back)
                return 0
                ;;
            view)
                if [ -f "$file" ]; then
                    $DIALOG --textbox "$file" 30 100
                else
                    $DIALOG --msgbox "❌ File not found: $file" 10 60
                fi
                ;;
            edit)
                if [ -f "$file" ]; then
                    ${EDITOR:-nano} "$file"
                    "$SCRIPT_DIR/persona-compile.sh" sync
                    $DIALOG --msgbox "✅ Saved & synced!\n\nChanges compiled to database." 10 60
                else
                    $DIALOG --msgbox "❌ File not found: $file" 10 60
                fi
                ;;
            activate)
                "$SCRIPT_DIR/persona-manager.sh" activate "$name"
                $DIALOG --msgbox "✅ Persona activated!\n\nFrequency: ~p.$name\n\nSwitch back: ~p.clear" 20 60
                ;;
            query)
                local field=$($DIALOG --title "Query Field" \
                    --menu "Select field:" 15 60 8 \
                    "identity" "Identity name" \
                    "title" "Identity title" \
                    "vibe" "Personality vibe" \
                    "emoji" "Allowed emoji" \
                    "voice" "Voice & tone" \
                    "rules" "Behavioral rules" \
                    "all" "Show all fields" \
                    3>&1 1>&2 2>&3)
                
                if [ -n "$field" ]; then
                    local result=$("$SCRIPT_DIR/persona-query.sh" "$field" "$name" 2>/dev/null || echo "Query not available")
                    $DIALOG --msgbox "$result" 20 70
                fi
                ;;
            unload)
                "$SCRIPT_DIR/persona-compile.sh" unload "$name"
                $DIALOG --msgbox "✅ Unloaded from DB\n\nMD file preserved at:\n$file" 12 60
                ;;
            delete)
                if $DIALOG --title "⚠️  Confirm Delete" \
                         --yesno "Delete persona file:\n\n$name.persona\n\nThis cannot be undone!" 12 60; then
                    rm -f "$file"
                    "$SCRIPT_DIR/persona-compile.sh" sync
                    $DIALOG --msgbox "✅ Deleted: $name" 10 60
                    return 0
                fi
                ;;
        esac
    done
}

# Show welcome screen
clear
echo -e "\033[1;36m╔═══════════════════════════════════════════════════════════════╗\033[0m"
echo -e "\033[1;36m║  🎭 Persona TUI v3.2                                          ║\033[0m"
echo -e "\033[1;36m║  Terminal GUI for persona management                           ║\033[0m"
echo -e "\033[1;36m╚═══════════════════════════════════════════════════════════════╝\033[0m"
echo ""
echo "Tip: You can also use quick commands: ~p | ~m | mpm status"
echo "Starting TUI..."
echo ""

# Sync on start
[ -f "$SCRIPT_DIR/persona-compile.sh" ] && "$SCRIPT_DIR/persona-compile.sh" sync 2>&1 | head -5

# Start main menu
main_menu
