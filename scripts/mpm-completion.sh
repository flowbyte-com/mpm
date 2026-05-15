# mpm-completion.sh — Tag autocomplete for bash/zsh
# Source this file (or source it from your .bashrc/.zshrc):
#   source /path/to/mpm-completion.sh
#
# Requires: MPM_WORKSPACE set to your MPM project directory
# Adds TAB completion for the hidden _suggest_tags command.
#
# Installation (bash):
#   echo "source /home/v/workspace/projects/mpm/scripts/mpm-completion.sh" >> ~/.bashrc
#
# Installation (zsh):
#   Add same to your .zshrc (zsh handles bash completions if emulating bash)
#
# After sourcing, TAB will suggest tag completions when typing:
#   mpm add "content" --tag <TAB>
#   mpm memory add "content" --tag <TAB>
#   mpm _suggest_tags <prefix><TAB>

_MPM_COMPLETE_TAGS_CMD="${MPM_WORKSPACE:-/home/v/workspace/projects/mpm}/bin/mpm _suggest_tags"

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