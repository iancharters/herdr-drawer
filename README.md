# herdr-drawer

![herdr-drawer demo](assets/demo.gif)

Persistent, multi-pane terminal drawers and configurable utility sidecars for [herdr](https://herdr.dev), scoped to the current tab.

Press `Ctrl+/` from Neovim (or any pane) to toggle a bottom terminal drawer in your editor's current working directory. Split the drawer into multiple panes, run commands, and press `Ctrl+/` again to hide the drawer — **without zooming your editor pane or disturbing sibling panes** (like a right-hand AI agent sidecar). You can also define named utility profiles (such as a full-height right-hand `cursor agent` or `opencode` sidecar on `Ctrl+Shift+/`) that coexist cleanly with your bottom drawer regardless of the order in which you open them.

---

## Requirements

- `herdr` `>= 0.7.0`
- macOS or Linux

---

## Quickstart

### 1. Install the Plugin

```bash
herdr plugin install iancharters/herdr-drawer
```

Or from a local checkout:

```bash
git clone https://github.com/iancharters/herdr-drawer.git ~/dev/herdr-plugins/herdr-drawer
cd ~/dev/herdr-plugins/herdr-drawer
make link
```

### 2. Bind `Ctrl+/` in `~/.config/herdr/config.toml`

```toml
# Toggle bottom terminal drawer
[[keys.command]]
key = "ctrl+/"
type = "plugin_action"
command = "herdr-drawer.toggle"
description = "toggle terminal drawer"
```

Without any profile config file, `herdr-drawer` works out of the box with a persistent bottom drawer (`30%` height) anchored beneath your active pane.

*(Optional)* Bind dedicated drawer split shortcuts, or simply use your standard `herdr` split keybindings (`prefix+v`, `prefix+s`, etc.) inside the drawer:

```toml
[[keys.command]]
key = "prefix+alt+v"
type = "plugin_action"
command = "herdr-drawer.split-right"
description = "split terminal drawer right"

[[keys.command]]
key = "prefix+alt+s"
type = "plugin_action"
command = "herdr-drawer.split-down"
description = "split terminal drawer down"
```

Reload `herdr`'s configuration (`prefix+shift+r`) after saving `config.toml`.

### 3. Configure Neovim *(Optional)*

To keep the drawer synchronized with Neovim's active working directory (`:cd`, `:lcd`, `:tcd`) and install `:HerdrDrawer*` commands, run:

```bash
# Inside herdr (opens an interactive setup popup):
herdr plugin action invoke herdr-drawer.setup

# Or from a local checkout:
make setup
```

Press `Enter` to accept the default path (`~/.config/nvim/lua/plugins/herdr-drawer.lua`) and restart Neovim.

---

## Configurable Utility Profiles & Starter Recipes

You can define any number of named utility profiles in:

```text
~/.config/herdr/plugins/config/herdr-drawer/profiles.toml
```

See [`profiles.example.toml`](profiles.example.toml) for a ready-to-copy template.

### Recipe 1: Neovim Bottom Drawer + Full-Height Cursor Agent Sidecar

This setup gives you:
- **`Ctrl+/`**: A multi-pane bottom terminal drawer (`30%` height) anchored directly beneath `nvim`.
- **`Ctrl+Shift+/`**: A persistent `cursor agent` sidecar (`35%` width) spanning the full right side of the tab (`scope = "tab"`), launched directly without echoing `cursor agent` in the terminal.

**`~/.config/herdr/plugins/config/herdr-drawer/profiles.toml`:**

```toml
default_profile = "default"

[profiles.default]
placement = "down"
scope = "anchor"
size_ratio = 0.30
persist_on_hide = true
anchor_policy = "app_in_tab_or_tab_scope"
anchor_app_match = ["nvim", "vim"]

[profiles.cursorRight]
placement = "right"
scope = "tab"
size_ratio = 0.35
persist_on_hide = true
show_command_text = false
command = "cursor agent"
```

**`~/.config/herdr/config.toml`:**

```toml
[[keys.command]]
key = "ctrl+/"
type = "plugin_action"
command = "herdr-drawer.toggle"
description = "toggle bottom terminal drawer"

[[keys.command]]
key = "ctrl+shift+/"
type = "shell"
command = "~/.config/herdr/plugins/iancharters/herdr-drawer/bin/herdr-drawer toggle --profile cursorRight"
description = "toggle right-hand Cursor Agent sidecar"
```

*(If installed via `make link` from a local checkout, point `command` to your checkout's `bin/herdr-drawer` path.)*

### Recipe 2: OpenCode / Claude Code Sidecar with `{{cwd}}` Interpolation

Use `{{cwd}}` (or `${cwd}`) in `command` to pass the anchor pane's working directory to your tool on initial session creation:

```toml
[profiles.opencodeRight]
placement = "right"
scope = "tab"
size_ratio = 0.35
persist_on_hide = true
show_command_text = false
command = "opencode {{cwd}}"
```

### Recipe 3: Ephemeral Test / Task Runner (`persist_on_hide = false`)

Set `persist_on_hide = false` and `show_command_text = true` for a drawer that runs a command visibly on open and terminates when hidden:

```toml
[profiles.testRunner]
placement = "down"
scope = "anchor"
size_ratio = 0.25
persist_on_hide = false
show_command_text = true
command = "go test -v ./..."
```

---

## Profile Configuration Reference

| Option | Values | Default | Description |
| :--- | :--- | :--- | :--- |
| `placement` | `"down"`, `"up"`, `"left"`, `"right"` | `"down"` | Side of the anchor pane (or tab root) where the utility pane opens. |
| `scope` | `"anchor"`, `"tab"` | `"anchor"` | `"anchor"` splits only the target anchor pane; `"tab"` splits the full tab root so the pane spans the entire side of the tab. |
| `size_ratio` | `0.10` .. `0.90` | `0.30` | Fraction of the split occupied by the utility pane. |
| `persist_on_hide` | `true`, `false` | `true` | Keep PTY sessions alive in the background daemon when hidden (`true`), or terminate on hide (`false`). |
| `command` | string | `""` | Optional startup command run when a new session is created. Supports `{{cwd}}` and `${cwd}` placeholders. |
| `show_command_text` | `true`, `false` | `false` | When `false`, runs `command` directly without echoing the command string or pre-filling prompts. When `true`, sends the command visibly to an interactive login shell. |
| `anchor_policy` | `"generic_non_plugin"`, `"app_in_tab_or_tab_scope"` | `"generic_non_plugin"` | How `scope = "anchor"` picks its target pane. `"app_in_tab_or_tab_scope"` prefers a pane running an `anchor_app_match` process (e.g. `nvim`), anchors to a single non-plugin pane if only one exists, or falls back to spanning the non-plugin subtree if multiple shell panes exist. |
| `anchor_app_match` | `[]string` | `["nvim", "vim"]` | Foreground process names matched when `anchor_policy = "app_in_tab_or_tab_scope"`. |

### Cleanup Actions

To close open utility panes and terminate background daemon sessions on demand:

```bash
# Clean up drawer/utility sessions in the current workspace only
herdr plugin action invoke herdr-drawer.cleanup-workspace

# Clean up all drawer/utility sessions across all workspaces and stop the daemon
herdr plugin action invoke herdr-drawer.cleanup-all
```

---

## How It Works

### Visual Layout (`default` Bottom Drawer + `cursorRight` Tab Sidecar)

Whether you open the bottom drawer (`Ctrl+/`) first or the right-hand sidecar (`Ctrl+Shift+/`) first, `herdr-drawer` converges to the same layout:

```text
+------------------------------------------+------------------------+
|                                          |                        |
|                  Neovim                  |                        |
|                 (w1:p1)                  |      Cursor Agent      |
|                                          |    (cursorRight, 35%)  |
+--------------------+---------------------+      scope = "tab"     |
| > ls               | > git status        |                        |
|   Drawer 1 (term_A)|   Drawer 2 (term_B) |                        |
+--------------------+---------------------+------------------------+
```

- **Order-Invariant Geometry**: Opening a `scope = "tab"` profile stages and reconstructs existing tab splits so the sidecar always wraps the outer tab root, while `scope = "anchor"` profiles split directly beneath your editor pane.
- **Single Multiplexer Daemon (`herdr-drawer daemon`)**: One lightweight background process per `herdr` server multiplexes all profile PTY sessions (`map[session_id]*ptySession`) with a `128 KB` screen replay ring buffer per pane.
- **Event-Driven Lifecycle (`events.subscribe`)**: Subscribes to `pane.created`, `pane.closed`, `tab.closed`, and `workspace.closed` over `$HERDR_SOCKET_PATH`. Native Herdr splits inside a drawer are automatically adopted into persistent sessions, and closing a tab or workspace immediately reaps orphaned PTYs with zero polling when idle.

---

## Development

```bash
make build   # compile bin/herdr-drawer from source
make setup   # interactively install the Neovim plugin loader
make test    # run Go unit and integration tests
make vet     # run go vet
make link    # build and link local checkout into herdr
```

## License

[MIT](LICENSE)
