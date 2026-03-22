#!/bin/bash

# Install MPM/MPM dependencies
# Usage: ./install-deps.sh [--check]

echo "🔍 Checking MPM OS v3.2 Dependencies..."
echo "============================================="
echo ""

check_dep() {
    local cmd="$1"
    local name="$2"
    local pkg="$3"
    
    if command -v "$cmd" &>/dev/null; then
        echo "✅ $name"
    else
        echo "❌ $name (needs: $pkg)"
        MISSING+=("$pkg")
    fi
}

check_python_dep() {
    local module="$1"
    local name="$2"
    local pkg="$3"
    
    if python3 -c "import $module" 2>/dev/null; then
        echo "✅ $name"
    else
        echo "❌ $name (needs: pip install $pkg)"
        PY_MISSING+=("$pkg")
    fi
}

MISSING=()
PY_MISSING=()

echo "System Packages:"
check_dep "sqlite3" "sqlite3" "sqlite3"
check_dep "inotifywait" "inotify-tools" "inotify-tools"
check_dep "bc" "bc" "bc"

echo ""
echo "Python Packages:"
check_python_dep "tiktoken" "tiktoken" "tiktoken"

echo ""

if [ ${#MISSING[@]} -eq 0 ] && [ ${#PY_MISSING[@]} -eq 0 ]; then
    echo "✅ All dependencies satisfied!"
    exit 0
fi

echo "Missing: ${MISSING[*]}"
echo ""
read -p "Install missing dependencies? (y/n) " -n 1 -r
echo ""

if [[ $REPLY =~ ^[Yy]$ ]]; then
    sudo apt update
    sudo apt install -y "${MISSING[@]}"
    echo ""
    echo "✅ Dependencies installed!"
else
    echo "⚠️  Skipped. Install manually: sudo apt install ${MISSING[*]}"
fi