#!/bin/bash

# MPM Installation Script - Enhanced for SymAI Opcodes
# Usage: ./install.sh [--install|--uninstall|--status]

SCRIPT_DIR="$(cd "$(dirname "$(readlink -f "$0")")" && pwd)"
MPM_BIN="$SCRIPT_DIR/mpm.sh"
P_SELECT="$SCRIPT_DIR/persona-select.sh"
M_SELECT="$SCRIPT_DIR/mode-select.sh"
USER_BIN="$HOME/.local/bin"

show_status() {
 echo -e "\033[1;32m≈ FLOWBYTE OS v3.2 [Silicon-Native] ≈\033[0m"
 echo "=========================================="
 
 # Check mpm
 if command -v mpm &>/dev/null; then
 echo "✅ mpm : System Kernel active"
 else
 echo "❌ mpm : Kernel missing"
 fi

 # Check Opcodes
 [ -L "$USER_BIN/~p" ] && echo "✅ ~p : Persona Matrix active" || echo "❌ ~p : Persona Opcode missing"
 [ -L "$USER_BIN/~m" ] && echo "✅ ~m : Mode Matrix active" || echo "❌ ~m : Mode Opcode missing"
 
 # Check SymAI Components
 SYMAI_DIR="$HOME/.openclaw/workspace/symai"
 if [ -d "$SYMAI_DIR" ]; then
 echo ""
 echo "🔗 SymAI Stack:"
 [ -f "$SYMAI_DIR/symai_v3.py" ] && echo " ✅ symai_v3.py (core)"
 [ -f "$SYMAI_DIR/tagger.py" ] && echo " ✅ tagger.py (refinery)"
 fi
}

do_install() {
 echo "⚡ Establishing Silicon-Native Links..."
 mkdir -p "$USER_BIN"

 # Link Kernel
 ln -sf "$MPM_BIN" "$USER_BIN/mpm"
 
 # Link Opcodes
 ln -sf "$P_SELECT" "$USER_BIN/~p"
 ln -sf "$M_SELECT" "$USER_BIN/~m"

 chmod +x "$SCRIPT_DIR"/*.sh

 # PATH Check
 if [[ ":$PATH:" == *":$USER_BIN:"* ]]; then
 echo "✅ Installation complete! Try typing '~p' or '~m'."
 else
 echo "⚠️ $USER_BIN is not in your PATH. Add it to ~/.bashrc."
 fi
}

do_uninstall() {
 echo "⚡ Decommissioning Communication Channels..."
 rm -f "$USER_BIN/mpm" "$USER_BIN/~p" "$USER_BIN/~m"
 echo "✅ Channels closed."
}

# Main handler remains the same as your upload
case "${1:---status}" in
 --install|install) do_install ;;
 --uninstall|uninstall) do_uninstall ;;
 --status|status) show_status ;;
 *) echo "Usage: ./install.sh [install|uninstall|status]" ;;
esac