#!/bin/bash

# MPM Installation Script - Enhanced for SymAI Opcodes
# Usage: ./install.sh [--install|--uninstall|--status]

SCRIPT_DIR="$(cd "$(dirname "$(readlink -f "$0")")" && pwd)"
MPM_BIN="$SCRIPT_DIR/mpm.sh"
P_SELECT="$SCRIPT_DIR/persona-select.sh"
M_SELECT="$SCRIPT_DIR/mode-select.sh"
USER_BIN="$HOME/.local/bin"

# Detect correct workspace path
if [ -d "$HOME/.openclaw/workspace" ]; then
    WORKSPACE="$HOME/.openclaw/workspace"
elif [ -d "$HOME/.picoclaw/workspace" ]; then
    WORKSPACE="$HOME/.picoclaw/workspace"
else
    WORKSPACE="$HOME/.openclaw/workspace"
fi

show_status() {
 echo -e "\033[1;32m╔═══════════════════════════════════════════════════════════════╗\033[0m"
 echo -e "\033[1;32m║  ≈ FLOWBYTE OS v3.2 [Silicon-Native] ≈                        ║\033[0m"
 echo -e "\033[1;32m╚═══════════════════════════════════════════════════════════════╝\033[0m"
 echo ""
 
 # Check mpm
 if command -v mpm &>/dev/null; then
 echo "✅ mpm         : System Kernel active"
 else
 echo "❌ mpm         : Kernel missing"
 fi

 # Check Opcodes
 [ -L "$USER_BIN/~p" ] && echo "✅ ~p          : Persona Matrix active" || echo "❌ ~p          : Persona Opcode missing"
 [ -L "$USER_BIN/~m" ] && echo "✅ ~m          : Mode Matrix active" || echo "❌ ~m          : Mode Opcode missing"
 [ -L "$USER_BIN/~?" ] && echo "✅ ~?          : Discovery opcode active" || echo "❌ ~?          : Discovery opcode missing"
 
 # Check dependencies
 echo ""
 echo "📦 Dependencies:"
 for cmd in fzf sqlite3 inotifywait whiptail; do
 if command -v $cmd &>/dev/null; then
 echo "   ✅ $cmd"
 else
 echo "   ❌ $cmd (needs: sudo apt install $cmd or inotify-tools)"
 fi
 done
 
 # Check MPM structure
 echo ""
 echo "📁 MPM Structure:"
 if [ -d "$WORKSPACE/MPM" ]; then
 echo "   ✅ MPM root     $WORKSPACE/MPM"
 [ -d "$WORKSPACE/MPM/persona" ] && echo "   ✅ personas/    $(ls "$WORKSPACE/MPM/persona"/*.persona 2>/dev/null | wc -l) personas"
 [ -d "$WORKSPACE/MPM/mode" ] && echo "   ✅ modes/       $(ls "$WORKSPACE/MPM/mode"/*.mode 2>/dev/null | wc -l) modes"
 else
 echo "   ❌ MPM not initialized"
 fi
 
 # Show PATH status
 echo ""
 if [[ ":$PATH:" == *":$USER_BIN:"* ]]; then
 echo "✅ PATH configured: $USER_BIN"
 else
 echo "⚠️  Add to PATH: export PATH=\"\$HOME/.local/bin:\$PATH\""
 fi
}

do_install() {
 echo -e "\033[1;36m╔═══════════════════════════════════════════════════════════════╗\033[0m"
 echo -e "\033[1;36m║  ⚡ Establishing Silicon-Native Links...                     ║\033[0m"
 echo -e "\033[1;36m╚═══════════════════════════════════════════════════════════════╝\033[0m"
 
 mkdir -p "$USER_BIN"
 mkdir -p "$WORKSPACE/MPM/persona"
 mkdir -p "$WORKSPACE/MPM/mode"

 # Link Kernel
 ln -sf "$MPM_BIN" "$USER_BIN/mpm"
 echo "✅ mpm         → $USER_BIN/mpm"
 
 # Link Opcodes (using mpm as dispatcher)
 ln -sf "$MPM_BIN" "$USER_BIN/~p"
 ln -sf "$MPM_BIN" "$USER_BIN/~m"
 ln -sf "$MPM_BIN" "$USER_BIN/~?"
 echo "✅ ~p          → Persona selector"
 echo "✅ ~m          → Mode selector"
 echo "✅ ~?          → Discovery/help"

 chmod +x "$SCRIPT_DIR"/*.sh

 # PATH Check
 echo ""
 if [[ ":$PATH:" == *":$USER_BIN:"* ]]; then
 echo -e "\033[1;32m✅ Installation complete!\033[0m"
 echo ""
 echo "Quick start:"
 echo "  mpm help        - Show all commands"
 echo "  mpm status      - Show system heartbeat"
 echo "  mpm persona ls  - List personas"
 echo "  mpm mode ls     - List modes"
 echo "  ~p              - Interactive persona selector"
 echo "  ~m              - Interactive mode selector"
 else
 echo -e "\033[1;33m⚠️  Add $USER_BIN to your PATH:\033[0m"
 echo "   echo 'export PATH=\"\$HOME/.local/bin:\$PATH\"' >> ~/.bashrc"
 echo "   source ~/.bashrc"
 fi
}

do_uninstall() {
 echo -e "\033[1;33m╔═══════════════════════════════════════════════════════════════╗\033[0m"
 echo -e "\033[1;33m║  ⚡ Decommissioning Communication Channels...                  ║\033[0m"
 echo -e "\033[1;33m╚═══════════════════════════════════════════════════════════════╝\033[0m"
 
 rm -f "$USER_BIN/mpm" "$USER_BIN/~p" "$USER_BIN/~m" "$USER_BIN/~?"
 echo "✅ Channels closed."
 echo ""
 echo "Note: MPM data preserved at $WORKSPACE/MPM"
 echo "To completely remove: rm -rf $WORKSPACE/MPM"
}

# Main handler
case "${1:---status}" in
 --install|install) do_install ;;
 --uninstall|uninstall) do_uninstall ;;
 --status|status) show_status ;;
 *) 
 echo "Usage: ./install.sh [install|uninstall|status]"
 echo ""
 echo "  install    - Link all SymAI opcodes to ~/.local/bin"
 echo "  uninstall  - Remove all opcodes"
 echo "  status     - Check installation status"
 ;;
esac
