# mpm-completion.sh — Tag autocomplete for bash/zsh
# Source this file (or source it from your .bashrc/.zshrc):
#   source /path/to/mpm-completion.sh
#
# Requires: MPM_WORKSPACE set to your MPM project directory
# Adds TAB completion for the hidden _suggest_tags command.
#
# Installation (bash):
#   echo "source $MPM_WORKSPACE/scripts/mpm-completion.sh" >> ~/.bashrc
#
# Installation (zsh):
#   Add same to your .zshrc (zsh handles bash completions if emulating bash)
#
# After sourcing, TAB will suggest tag completions when typing:
#   mpm add "content" --tag <TAB>
#   mpm memory add "content" --tag <TAB>
#   mpm _suggest_tags <prefix><TAB>

# Resolve the mpm binary used for tag suggestions.
#
# Resolution order:
#   1. MPM_WORKSPACE — explicit project directory (set by operators
#      who cloned mpm to a non-default location). The completion runs
#      the in-source `.build/bin/mpm` binary which understands the
#      _suggest_tags command and works against the project's
#      workspace DB.
#
# Fallback: if MPM_WORKSPACE is unset, the completion warns loudly
# and skips registration. The previous behaviour silently fell back
# to the original author's checkout path, which pointed at a
# non-existent location for any other user and produced a confusing
# "_suggest_tags: command not found" mid-TAB instead of an actionable
# error at sourcing time.
if [ -z "${MPM_WORKSPACE:-}" ]; then
    echo "mpm-completion.sh: MPM_WORKSPACE is not set; skipping tag-completion registration." >&2
    echo "  export MPM_WORKSPACE=/path/to/mpm  (your mpm checkout)" >&2
    return 0 2>/dev/null || exit 0
fi

_MPM_COMPLETE_TAGS_CMD="${MPM_WORKSPACE}/bin/mpm _suggest_tags"

_mpm_tag_complete() {
    local cur prev words cword
    # Detect if we're mid-word (typing a tag prefix after --tag)
    cur="${COMP_WORDS[COMP_CWORD]}"
    prev="${COMP_WORDS[COMP_CWORD-1]}"

    # Only activate after --tag flag
    if [[ "$prev" == "--tag" ]] || [[ "$cur" == "--tag"* ]]; then
        # Strip --tag prefix if typed as one word
        local prefix="${cur#--tag}"
        if [[ "$prefix" == "" ]]; then
            prefix="${COMP_WORDS[COMP_CWORD]}"
        fi
        # Query tags and delegate to compgen
        COMPREPLY=( $(compgen -W "$(${_MPM_COMPLETE_TAGS_CMD} "$prefix" 2>/dev/null)" -- "$prefix") )
    fi
}

# Register completions for mpm command
complete -F _mpm_tag_complete mpm