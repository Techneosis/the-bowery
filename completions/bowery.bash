# bash completion for bowery: commands, agent names, and flags.
# Names come from the hidden `names` command (the dispatch records), so this
# never reads the records itself.

_bowery() {
    local cur prev cmd i
    cur=${COMP_WORDS[COMP_CWORD]}
    prev=${COMP_WORDS[COMP_CWORD-1]}

    # Everything after a literal `--` is the agent's prompt.
    for ((i = 1; i < COMP_CWORD; i++)); do
        [[ ${COMP_WORDS[i]} == -- ]] && return
    done

    cmd=${COMP_WORDS[1]}
    if ((COMP_CWORD == 1)); then
        COMPREPLY=($(compgen -W "dispatch done resume adopt orphans list mute unmute swarm-tab swarm-last config upgrade version" -- "$cur"))
        return
    fi

    case $prev in
        --cwd|--also-worktree) COMPREPLY=($(compgen -d -- "$cur")); return ;;
        --split) COMPREPLY=($(compgen -W "right down" -- "$cur")); return ;;
        --kind) COMPREPLY=($(compgen -W "claude codex" -- "$cur")); return ;;
        --machine)
            COMPREPLY=($(compgen -W "$(herdr machine list --json 2>/dev/null | sed -n 's/.*"label": *"\([^"]*\)".*/\1/p')" -- "$cur"))
            return ;;
        --worktree|--base|--pane|--from|--model|--timeout|--as|--launcher|--not-within) return ;;
    esac

    local names
    case $cmd in
        dispatch)
            [[ $cur == -* ]] && COMPREPLY=($(compgen -W "--swarm --space --split --tab --workspace --pane --from --cwd --worktree --base --also-worktree --machine --as --quiet --loud --gated --kind --model --launcher --timeout --wait --" -- "$cur")) ;;
        done)
            if [[ $cur == -* ]]; then
                COMPREPLY=($(compgen -W "--force --keep-worktree --keep-branch --dry-run --machine" -- "$cur"))
            else
                names=$(bowery names 2>/dev/null)
                COMPREPLY=($(compgen -W "$names" -- "$cur") $(compgen -d -- "$cur"))
            fi ;;
        adopt)
            if ((COMP_CWORD == 2)); then
                COMPREPLY=($(compgen -W "$(bowery names 2>/dev/null)" -- "$cur"))
            else
                COMPREPLY=($(compgen -d -- "$cur"))
            fi ;;
        resume)
            [[ $cur == -* ]] && COMPREPLY=($(compgen -W "--swarm --workspace --space --gated --ungated --launcher --no-launcher --fresh --if-dead --not-within --json --dry-run --timeout --machine --" -- "$cur")) \
                || COMPREPLY=($(compgen -W "$(bowery names 2>/dev/null)" -- "$cur")) ;;
        mute|unmute)
            [[ $cur == -* ]] && COMPREPLY=($(compgen -W "--machine" -- "$cur")) \
                || COMPREPLY=($(compgen -W "$(bowery names 2>/dev/null)" -- "$cur")) ;;
        orphans) COMPREPLY=($(compgen -W "--machine --json" -- "$cur")) ;;
        list) COMPREPLY=($(compgen -W "--machine" -- "$cur")) ;;
        swarm-tab) [[ $cur == -* ]] && COMPREPLY=($(compgen -W "--cwd --machine" -- "$cur")) ;;
        upgrade) COMPREPLY=($(compgen -W "--check" -- "$cur")) ;;
    esac
}
complete -F _bowery bowery
