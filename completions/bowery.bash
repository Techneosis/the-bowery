# bash completion for bowery and the office host: commands, agent names, and flags.
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
        COMPREPLY=($(compgen -W "dispatch done resume adopt orphans list mute unmute swarm-tab swarm-last watch place queue peers office config upgrade version" -- "$cur"))
        return
    fi

    case $prev in
        --cwd|--also-worktree) COMPREPLY=($(compgen -d -- "$cur")); return ;;
        --split) COMPREPLY=($(compgen -W "right down" -- "$cur")); return ;;
        --kind) COMPREPLY=($(compgen -W "claude codex" -- "$cur")); return ;;
        --machine)
            COMPREPLY=($(compgen -W "$(herdr machine list --json 2>/dev/null | sed -n 's/.*"label": *"\([^"]*\)".*/\1/p')" -- "$cur"))
            return ;;
        --worktree|--base|--pane|--from|--model|--timeout|--as|--launcher|--not-within|--ticket|--space) return ;;
        --job-kind|--office|--pin|--scope|--area|--needs|--asked-by|--priority|--tag|--surface) return ;;
        --profile|--effects|--credit|--repo|--branch|--expires|--id|--name|--poll|--max|--since) return ;;
    esac

    local names
    case $cmd in
        dispatch)
            [[ $cur == -* ]] && COMPREPLY=($(compgen -W "--swarm --space --split --tab --workspace --pane --from --cwd --worktree --base --also-worktree --machine --as --ticket --quiet --loud --gated --kind --model --launcher --timeout --wait --no-start-check --job-kind --office --scope --needs --asked-by --priority --pin --tag --surface --" -- "$cur")) ;;
        place)
            [[ $cur == -* ]] && COMPREPLY=($(compgen -W "--retry --kind --scope --area --needs --effects --credit --asked-by --priority --pin --office --tag --surface --profile --repo --branch --base --expires --id --name --ticket --" -- "$cur")) ;;
        queue|peers) COMPREPLY=($(compgen -W "--json" -- "$cur")) ;;
        watch)
            if [[ $cur == -* ]]; then
                COMPREPLY=($(compgen -W "--poll --max --help" -- "$cur"))
            else
                COMPREPLY=($(compgen -f -- "$cur"))
            fi ;;
        office)
            if ((COMP_CWORD == 2)); then
                COMPREPLY=($(compgen -W "ls describe log stop" -- "$cur"))
            elif [[ $cur == -* ]]; then
                COMPREPLY=($(compgen -W "--json --since" -- "$cur"))
            fi ;;
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
        list) COMPREPLY=($(compgen -W "--machine --json" -- "$cur")) ;;
        swarm-tab) [[ $cur == -* ]] && COMPREPLY=($(compgen -W "--cwd --machine" -- "$cur")) ;;
        upgrade) COMPREPLY=($(compgen -W "--check" -- "$cur")) ;;
    esac
}
complete -F _bowery bowery

# The office host. Office names are directories under its config root.
_office() {
    local cur prev sub root
    cur=${COMP_WORDS[COMP_CWORD]}
    prev=${COMP_WORDS[COMP_CWORD-1]}
    if ((COMP_CWORD == 1)); then
        COMPREPLY=($(compgen -W "ls describe open close clear stop done yielded pull log record check publish serve bus private version" -- "$cur"))
        return
    fi
    case $prev in
        --until|--kinds|--scope|--interval|--backstop|--for|--since|--valid) return ;;
        --owner|--subject|--actor|--data|--cause|--cursor|--topic) return ;;
        --type) COMPREPLY=($(compgen -W "card request ticket hide note" -- "$cur")); return ;;
    esac
    sub=${COMP_WORDS[1]}
    if [[ $sub == bus ]]; then
        if ((COMP_CWORD == 2)); then
            COMPREPLY=($(compgen -W "serve publish follow status" -- "$cur"))
        elif [[ $cur == -* ]]; then
            case ${COMP_WORDS[2]} in
                publish) COMPREPLY=($(compgen -W "--topic --owner --subject --actor --data --cause --json" -- "$cur")) ;;
                follow) COMPREPLY=($(compgen -W "--cursor --topic --once" -- "$cur")) ;;
                status) COMPREPLY=($(compgen -W "--json" -- "$cur")) ;;
            esac
        fi
        return
    fi
    if [[ $sub == private ]]; then
        if ((COMP_CWORD == 2)); then
            COMPREPLY=($(compgen -W "ls show" -- "$cur"))
        elif [[ $cur == -* ]]; then
            case ${COMP_WORDS[2]} in
                ls) COMPREPLY=($(compgen -W "--type --json" -- "$cur")) ;;
                show) COMPREPLY=($(compgen -W "--json" -- "$cur")) ;;
            esac
        fi
        return
    fi
    if [[ $cur == -* ]]; then
        case $sub in
            ls) COMPREPLY=($(compgen -W "--json" -- "$cur")) ;;
            describe) COMPREPLY=($(compgen -W "--json --probe" -- "$cur")) ;;
            open|close) COMPREPLY=($(compgen -W "--until --kinds --scope $([[ $sub == close ]] && echo --now)" -- "$cur")) ;;
            pull) COMPREPLY=($(compgen -W "--json --loop --interval --backstop --for" -- "$cur")) ;;
            log) COMPREPLY=($(compgen -W "--since --json" -- "$cur")) ;;
            publish) COMPREPLY=($(compgen -W "--valid --json" -- "$cur")) ;;
            serve) COMPREPLY=($(compgen -W "--listen" -- "$cur")) ;;
        esac
        return
    fi
    if ((COMP_CWORD == 2)) && [[ $sub != ls && $sub != publish && $sub != serve && $sub != version ]]; then
        root=${OFFICE_ROOT:+$OFFICE_ROOT/etc}
        root=${root:-/usr/local/etc/offices}
        COMPREPLY=($(compgen -W "$(for d in "$root"/*/; do [[ -f $d/office.toml ]] && basename "$d"; done 2>/dev/null)" -- "$cur"))
    fi
}
complete -F _office office
