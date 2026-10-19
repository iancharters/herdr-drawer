if vim.g.loaded_herdr_drawer then
  return {}
end

if not vim.env.HERDR_PANE_ID or vim.env.HERDR_PANE_ID == "" then
  return {}
end

vim.g.loaded_herdr_drawer = 1

local uv = vim.uv or vim.loop

local function get_uid()
  if uv and uv.getuid then
    return uv.getuid()
  end
  return tonumber(vim.fn.system("id -u")) or 0
end

local state_dir = vim.env.HERDR_DRAWER_STATE_DIR
if not state_dir or state_dir == "" then
  state_dir = string.format("/tmp/herdr-drawer-%d", get_uid())
end
local cwd_dir = state_dir .. "/cwd"
local sanitized_pane_id = (vim.env.HERDR_PANE_ID or ""):gsub(":", "_")

local function sync_nvim_cwd()
  local cwd = vim.fn.getcwd()
  if not cwd or cwd == "" then
    return
  end
  vim.fn.mkdir(cwd_dir, "p", "0700")
  local f = io.open(cwd_dir .. "/" .. sanitized_pane_id, "w")
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

local script_source = debug.getinfo(1, "S").source
local self_plugin_root = nil
if type(script_source) == "string" and script_source:sub(1, 1) == "@" then
  self_plugin_root = vim.fn.fnamemodify(script_source:sub(2), ":p:h:h")
end

local cached_bin = nil
local function resolve_drawer_bin()
  if vim.g.herdr_drawer_bin and vim.g.herdr_drawer_bin ~= "" then
    return vim.g.herdr_drawer_bin
  end
  if cached_bin and vim.fn.executable(cached_bin) == 1 then
    return cached_bin
  end

  if self_plugin_root then
    local candidate = self_plugin_root .. "/bin/herdr-drawer"
    if vim.fn.executable(candidate) == 1 then
      cached_bin = candidate
      return cached_bin
    end
  end

  local config_dir = vim.env.XDG_CONFIG_HOME
  if not config_dir or config_dir == "" then
    config_dir = vim.fn.expand("~/.config")
  end
  local plugins_json = config_dir .. "/herdr/plugins.json"
  if vim.fn.filereadable(plugins_json) == 1 then
    local ok, lines = pcall(vim.fn.readfile, plugins_json)
    if ok and lines then
      local decoded_ok, plugins = pcall(vim.json.decode, table.concat(lines, "\n"))
      if decoded_ok and type(plugins) == "table" then
        for _, p in ipairs(plugins) do
          if
            type(p) == "table"
            and p.plugin_root
            and (
              p.plugin_id == "herdr-drawer"
              or (type(p.plugin_id) == "string" and p.plugin_id:match("%.herdr%-drawer$"))
            )
          then
            local bin = p.plugin_root .. "/bin/herdr-drawer"
            if vim.fn.executable(bin) == 1 then
              cached_bin = bin
              return cached_bin
            end
          end
        end
      end
    end
  end

  return nil
end

local function run_drawer(action, extra_args)
  sync_nvim_cwd()
  local bin = resolve_drawer_bin()
  if not bin then
    vim.notify("herdr-drawer: binary not found in herdr plugin directory", vim.log.levels.WARN)
    return
  end
  local cmd = {
    bin,
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

return {}
