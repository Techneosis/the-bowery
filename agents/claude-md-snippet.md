## Dispatching herdr agents

Starting a separate herdr agent (its own pane and Claude session) to hand work
to is **The Bowery** (`bowery`): load the **`bowery`** skill first. Never
hand-roll `herdr pane split` + `herdr agent start` for it, and never use
`herdr worktree create` for a dispatched agent's checkout. Tear each agent
down with `bowery done <name>` as soon as its result is verified; `bowery
orphans` shows what was left behind. Work that belongs to an office goes
through `bowery place` (read its exit: 0 placed, 4 waiting, 3 refused, 1
error), and agents, PRs and CI runs in flight are watched with `bowery
watch`, not a polling loop; both are in the skill.
