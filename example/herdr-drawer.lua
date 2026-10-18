-- herdr-drawer.lua — Neovim integration for herdr-drawer
--
-- Copy this file into your Neovim config:
--   cp example/herdr-drawer.lua ~/.config/nvim/plugin/herdr-drawer.lua
-- or source it from your config (e.g. init.lua or lua/config/keymaps.lua):
--   dofile(vim.fn.expand("~/dev/herdr-plugins/herdr-drawer/example/herdr-drawer.lua"))
--
-- What this file does:
-- 1. Automatically syncs Neovim's current working directory (`vim.fn.getcwd()`)
--    for `$HERDR_PANE_ID` so that when `herdr` intercepts `Ctrl+/` at the
--    multiplexer level, the drawer always opens in Neovim's active directory
--    (including after `:cd`, `:lcd`, or `:tcd`).
-- 2. Registers `:HerdrDrawerToggle`, `:HerdrDrawerOpen`, `:HerdrDrawerClose`,
--    `:HerdrDrawerSplitRight`, and `:HerdrDrawerSplitDown` commands, plus
--    `<C-/>` / `<C-_>` fallback keymaps.

if not vim.env.HERDR_PANE_ID or vim.env.HERDR_PANE_ID == "" then
  return
end

local uv = vim.uv or vim.loop

local function get_uid()
  if uv and uv.getuid then
    return uv.getuid()
  end
  return tonumber(vim.fn.system("id -u")) or 0
end

local state_dir = string.format("/tmp/herdr-drawer-%d", get_uid())
local cwd_dir = state_dir .. "/cwd"
local sanitized_pane_id = (vim.env.HERDR_PANE_ID or ""):gsub(":", "_")

local function sync_nvim_cwd()
  local cwd = vim.fn.getcwd()
  if not cwd or cwd == "" then
    return
  end
  vim.fn.mkdir(cwd_dir, "p", "0700")
  local path = cwd_dir .. "/" .. sanitized_pane_id
  local f = io.open(path, "w")
  if f then
    f:write(cwd)
    f:close()
  end
end

local group = vim.api.nvim_create_augroup("HerdrDrawerCwdSync", { clear = true })
vim.api.nvim_create_autocmd({ "VimEnter", "DirChanged", "BufEnter", "WinEnter" }, {
  group = group,
  callback = sync_nvim_cwd,
})
sync_nvim_cwd()

local cached_runner = nil
local function resolve_drawer_runner()
  if vim.g.herdr_drawer_bin and vim.g.herdr_drawer_bin ~= "" then
    return vim.g.herdr_drawer_bin
  end
  if cached_runner then
    return cached_runner
  end

  local plugins_json = vim.fn.expand("~/.config/herdr/plugins.json")
  if vim.fn.filereadable(plugins_json) == 1 then
    local ok, content = pcall(vim.fn.readfile, plugins_json)
    if ok and content then
      local decoded_ok, plugins = pcall(vim.json.decode, table.concat(content, "\n"))
      if decoded_ok and type(plugins) == "table" then
        for _, p in ipairs(plugins) do
          if type(p) == "table" and p.plugin_root and (p.plugin_id == "herdr-drawer" or (type(p.plugin_id) == "string" and p.plugin_id:match("%.herdr%-drawer$"))) then
            local bin = p.plugin_root .. "/bin/herdr-drawer"
            if vim.fn.executable(bin) == 1 then
              cached_runner = bin
              return cached_runner
            end
            local script = p.plugin_root .. "/drawer.sh"
            if vim.fn.filereadable(script) == 1 then
              cached_runner = script
              return cached_runner
            end
          end
        end
      end
    end
  end

  local fallback = vim.fn.expand("~/dev/herdr-plugins/herdr-drawer/drawer.sh")
  cached_runner = fallback
  return cached_runner
end

local function run_drawer(action, extra_args)
  sync_nvim_cwd()
  local runner = resolve_drawer_runner()
  local cmd = {
    runner,
    action,
    "--from-nvim",
    "--pane",
    vim.env.HERDR_PANE_ID,
    "--cwd",
    vim.fn.getcwd(),
  }
  if extra_args then
    for _, arg in ipairs(extra_args) do
      table.insert(cmd, arg)
    end
  end
  vim.fn.jobstart(cmd, { detach = true })
end

vim.api.nvim_create_user_command("HerdrDrawerToggle", function()
  run_drawer("toggle")
end, { desc = "Toggle herdr terminal drawer" })

vim.api.nvim_create_user_command("HerdrDrawerOpen", function()
  run_drawer("open")
end, { desc = "Open herdr terminal drawer" })

vim.api.nvim_create_user_command("HerdrDrawerClose", function()
  run_drawer("close")
end, { desc = "Close herdr terminal drawer" })

vim.api.nvim_create_user_command("HerdrDrawerSplitRight", function()
  run_drawer("split", { "--direction", "right" })
end, { desc = "Split herdr drawer pane right" })

vim.api.nvim_create_user_command("HerdrDrawerSplitDown", function()
  run_drawer("split", { "--direction", "down" })
end, { desc = "Split herdr drawer pane down" })

vim.keymap.set({ "n", "t" }, "<C-/>", function()
  run_drawer("toggle")
end, { desc = "Toggle herdr terminal drawer", silent = true })

vim.keymap.set({ "n", "t" }, "<C-_>", function()
  run_drawer("toggle")
end, { desc = "Toggle herdr terminal drawer", silent = true })
