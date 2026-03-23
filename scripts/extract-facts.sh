#!/bin/bash

# Fact Extractor - Pull only facts/decisions from memory files
# Usage: ./extract-facts.sh <input-file> [--output <file>]

INPUT_FILE=""
OUTPUT_FILE=""

while [[ $# -gt 0 ]]; do
    case $1 in
        --output) OUTPUT_FILE="$2"; shift 2 ;;
        *) 
            if [ -z "$INPUT_FILE" ]; then
                INPUT_FILE="$1"
            fi
            shift
            ;;
    esac
done

if [ -z "$INPUT_FILE" ]; then
    echo "Usage: ./extract-facts.sh <file> [--output <file>]"
    exit 1
fi

if [ ! -f "$INPUT_FILE" ]; then
    echo "❌ File not found: $INPUT_FILE"
    exit 1
fi

echo "🔍 Extracting facts from: $INPUT_FILE"

# Extract patterns that indicate facts/decisions
extract_facts() {
    cat "$1" | \
    # Keep lines with these patterns (facts/decisions/configs)
    grep -E '(Already|Configured|Set to|Found at|Located|Installed|Enabled|Disabled|Active|Inactive|Created|Status:|Path:|User:|Password:|Token:|API|Key:|URL:|http|Recommendation|Conclusion|Summary|Important|Note:|Warning|Current setup|Your current|So your|This means|Result:|Output:|Size:|Version:|Port:|Host:)' | \
    # Keep bullet points with actual info
    grep -E '^\s*[-*]\s*\*?[A-Z]' | \
    # Keep numbered lists
    grep -E '^\s*[0-9]+\.\s*\*?[A-Z]' | \
    # Keep code lines that look like paths/commands
    grep -E '^(\s{4}|/|cd |mkdir |cp |mv |rm )' | \
    # Remove lines that are questions
    grep -vE '\?$' | \
    # Remove lines that are suggestions/offers
    grep -vE '(Want|Shall|Should|Could|Would|Let me|I can|I will|Or )' | \
    # Remove emoji-heavy lines
    grep -vE '^[^a-zA-Z]*[✅❌🔍📊][^a-zA-Z]*$' | \
    # Clean up leading/trailing whitespace
    sed -E 's/^[[:space:]]+//; s/[[:space:]]+$//' | \
    # Remove duplicate lines
    sort -u
}

if [ -n "$OUTPUT_FILE" ]; then
    extract_facts "$INPUT_FILE" > "$OUTPUT_FILE"
    echo "✅ Extracted facts: $OUTPUT_FILE"
    echo "Lines: $(wc -l < "$OUTPUT_FILE")"
else
    extract_facts "$INPUT_FILE"
fi
