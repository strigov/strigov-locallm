<div align="center">

# 🦙 locallm

**Your gaming PC becomes a private AI server for all your other computers.**

One Windows PC with an NVIDIA graphics card runs local language models.<br>
Your laptops use them from anywhere — in [opencode](https://opencode.ai), [pi](https://github.com/earendil-works/pi) or any OpenAI-compatible app.

**English** · [Русский](README.ru.md)

![Windows 11](https://img.shields.io/badge/host-Windows%2011-0078D4)
![NVIDIA](https://img.shields.io/badge/GPU-NVIDIA-76B900)
![llama.cpp](https://img.shields.io/badge/engine-llama.cpp-8A2BE2)
![Tailscale](https://img.shields.io/badge/network-Tailscale-242424)
![version](https://img.shields.io/badge/version-0.2.4-informational)

</div>

---

## Contents

- [What is this?](#what-is-this)
- [How it works](#how-it-works)
- [What you need](#what-you-need)
- [Part 1 — Set up the host PC](#part-1--set-up-the-host-pc)
- [Part 2 — Connect other computers](#part-2--connect-other-computers)
  - [opencode](#opencode)
  - [pi](#pi)
  - [Any other app](#any-other-app)
- [Everyday use](#everyday-use)
- [Troubleshooting](#troubleshooting)
- [Update and uninstall](#update-and-uninstall)
- [Security](#security)
- [More documentation](#more-documentation)

---

## What is this?

Running a good language model needs a big graphics card. Your laptop probably doesn't have one, but the desktop PC at home might.

**locallm** is a small Windows program (`locallm-host.exe`) that turns that PC into a model server:

- 🧠 **Runs one model at a time** in video memory, using [llama.cpp](https://github.com/ggml-org/llama.cpp).
- 🔄 **Switches models by itself.** Ask for `gemma` while `qwen-coder` is loaded — the host unloads one, loads the other, and answers. Two models never fight for video memory.
- 🌐 **Is reachable from your other computers** through [Tailscale](https://tailscale.com), a free private network. No ports opened to the internet.
- 🔑 **Asks for a key.** Only computers that have your key can use it.
- 🖱️ **Has a tray icon and a web panel.** Free the graphics card for a game with one click, add new models without editing files.

> **Who sits at the PC matters too.** The host is built for a desktop someone actually uses. "Pause" and "Unload model" give the graphics card back to games or video editing at any moment.

## How it works

```
  Laptop / Mac / another PC                 Windows PC with NVIDIA GPU
 ┌──────────────────────────┐             ┌────────────────────────────────┐
 │ opencode, pi, any app    │             │ locallm-host.exe  (tray icon)  │
 │   "model": "gemma"       │  Tailscale  │   checks the key               │
 │   key from               │ ──────────▶ │   loads / switches the model   │
 │   ~/.config/locallm/     │  private,   │              │                 │
 │   api-key                │  encrypted  │              ▼                 │
 └──────────────────────────┘             │ llama-server.exe (llama.cpp)   │
                                          │   one model in video memory    │
                                          └────────────────────────────────┘
```

A few words you will see below:

| Word | Meaning |
|---|---|
| **Host** | The Windows PC with the graphics card, and the `locallm-host.exe` program on it. |
| **Client** | Any other computer (or app) that sends questions to the host. |
| **Model** | A `.gguf` file with a language model, e.g. downloaded from [Hugging Face](https://huggingface.co/models?library=gguf). |
| **VRAM** | Video memory on the graphics card. A model must fit into it. |
| **Tailnet** | Your private Tailscale network: all your devices signed into the same Tailscale account. |
| **API key** | A long random password the host creates for clients. |

## What you need

**On the host PC**

- Windows 11
- An NVIDIA graphics card (more VRAM = bigger models; 16–24 GB is comfortable)
- An up-to-date NVIDIA driver
- Around 20–50 GB of free disk space for models

**On each client computer**

- macOS, Linux or Windows
- [Tailscale](https://tailscale.com/download), signed into the **same** account as the host
- The app you want to use: [opencode](https://opencode.ai), [pi](https://github.com/earendil-works/pi), or anything that speaks the OpenAI API

---

## Part 1 — Set up the host PC

Everything here is done **once**, on the Windows PC. Open **Windows Terminal** (right‑click Start → Terminal) for the commands.

### Step 1. Install the tools

Copy this block into the terminal and press Enter. It installs PowerShell 7, Git, Go (only needed to build the program) and Tailscale:

```powershell
winget install --id Microsoft.PowerShell -e
winget install --id Git.Git -e
winget install --id GoLang.Go -e
winget install --id Tailscale.Tailscale -e
```

Then **close the terminal and open it again**, so it sees the new programs. Start Tailscale from the Start menu and sign in.

### Step 2. Get llama.cpp

llama.cpp is the engine that actually runs models.

1. Open the [llama.cpp releases page](https://github.com/ggml-org/llama.cpp/releases/latest).
2. Download two archives for Windows with **CUDA**: `llama-…-bin-win-cuda-…-x64.zip` and the matching `cudart-llama-bin-win-cuda-…-x64.zip`.
3. Unpack **both** into the same folder `C:\llama\`. You should now have `C:\llama\llama-server.exe`.

### Step 3. Download a model

Models are `.gguf` files. Put them into `C:\models\` (subfolders are fine).

- Search [Hugging Face for GGUF models](https://huggingface.co/models?library=gguf&sort=trending).
- **Rule of thumb:** the file should be about 2–4 GB **smaller** than your VRAM. The rest is needed for the conversation itself ("context").
- Names like `Q4_K_M`, `Q4_0`, `IQ3_XXS` are compression levels. `Q4` is a good default.
- Some models that understand pictures have a second small file, `mmproj-….gguf`. Download it too.

### Step 4. Install locallm

```powershell
cd $HOME
git clone https://github.com/strigov/strigov-locallm.git
cd strigov-locallm
pwsh -File scripts\install.ps1
```

On first install the script asks for two paths — type them in full, starting with the drive letter:

- **llama.cpp folder** — the one containing `llama-server.exe`, e.g. `D:\AI\llama`;
- **models folder** — where your `.gguf` files are, e.g. `D:\AI\models`.

You can copy a path from the File Explorer address bar. Pressing Enter with no answer means `C:\llama` and `C:\models`. To skip the questions, pass the paths up front:

```powershell
pwsh -File scripts\install.ps1 -LlamaDir 'D:\AI\llama' -ModelsDir 'D:\AI\models'
```

What happens:

- the program is built and copied to `%LOCALAPPDATA%\Programs\locallm\`;
- settings are created in `%LOCALAPPDATA%\locallm\`;
- a **Start → locallm** shortcut appears;
- a 🦙 **llama icon** appears next to the clock (if you don't see it, click the `˄` arrow), and the **web panel** opens in your browser.

### Step 5. Add your models

> The tray menu and the web panel speak English, Russian and Chinese. The first start picks the language of Windows; change it any time in the tray (🦙 → **Language**) or in the list at the top right of the panel. Below, buttons are named as in the English interface.

In the web panel:

1. Click **Find .gguf files and add a model**. You'll see every `.gguf` file in `C:\models\`.
2. Click **Choose** next to a model. The editor opens with a suggested name, e.g. `qwen-coder`.
3. *(Optional)* Change the name. **Clients will use exactly this name**, so keep it short: letters, digits, `.`, `_`, `-`.
4. *(Optional)* Add flags, one per line. The most useful one is the context size: `-c` on one line and `32768` on the next.
5. Click **Save**. The model is available immediately — no restart.

Click **Load** next to it to test. The icon turns 🟡 yellow while loading and 🟢 green when ready.

> **Profiles** are named variants of the same model with different flags, e.g. a `long` profile with a bigger context. A profile `long` on model `qwen-coder` appears to clients as a separate model `qwen-coder-long`.

### Step 6. Make the host visible to your other computers

The host only listens on the PC itself. This one command lets Tailscale forward it to your tailnet (no admin rights needed, survives reboots):

```powershell
tailscale serve --bg --http=8080 http://127.0.0.1:8080
tailscale serve status
```

The second command prints your **host address**, something like:

```
http://my-pc.tail1234.ts.net:8080
```

📝 **Write it down** — every client needs it.

> ⚠️ **Never run `tailscale funnel`.** Funnel exposes a service to the whole internet. `serve` keeps it inside your private tailnet.

### Step 7. Turn on autostart (optional)

Click the 🦙 tray icon → **Start when signing in to Windows**.

---

## Part 2 — Connect other computers

Do this on **every** laptop or computer that should use the models. Commands below are for **macOS / Linux** Terminal; Windows variants follow each block.

### Step 1. Join the tailnet

Install [Tailscale](https://tailscale.com/download) and sign in with the **same account** as the host PC.

Check that the host is reachable (replace the address with yours):

```bash
curl http://my-pc.tail1234.ts.net:8080/health
```

Expected answer: `{"status":"ok"}`.

> If you get `Could not resolve host`: click the Tailscale icon in the menu bar and enable **"Use Tailscale DNS settings"**.

### Step 2. Copy the API key

The host created the key on first start in `%LOCALAPPDATA%\locallm\keys.txt`. Every client stores it in the same place: `~/.config/locallm/api-key`. Apps read it from there, so the key never appears in config files.

**Easiest way — Taildrop** (Tailscale's file sending). On the **host PC**, replace `my-laptop` with the client's name from the Tailscale app (the colon and the quotes are required):

```powershell
tailscale file cp "$env:LOCALAPPDATA\locallm\keys.txt" "my-laptop:"
```

On the **client**, the file lands in Downloads. This block moves the key into place and deletes the downloaded file:

```bash
mkdir -p ~/.config/locallm
grep -v '^#' ~/Downloads/keys.txt | head -n 1 | tr -d '\r\n ' > ~/.config/locallm/api-key
chmod 600 ~/.config/locallm/api-key
rm ~/Downloads/keys.txt
wc -c ~/.config/locallm/api-key
```

The last line should print `43` — the length of the key.

<details>
<summary><b>Windows client</b> (PowerShell)</summary>

```powershell
New-Item -ItemType Directory -Force "$HOME\.config\locallm" | Out-Null
$key = (Get-Content "$HOME\Downloads\keys.txt" | Where-Object { $_.Trim() -and -not $_.StartsWith('#') } | Select-Object -First 1).Trim()
[IO.File]::WriteAllText("$HOME\.config\locallm\api-key", $key)
Remove-Item "$HOME\Downloads\keys.txt"
(Get-Item "$HOME\.config\locallm\api-key").Length
```

If Taildrop isn't an option, open `keys.txt` on the host in Notepad and copy the long line (not the one starting with `#`) into the `api-key` file by hand.

</details>

**Test it** (replace the address):

```bash
H=http://my-pc.tail1234.ts.net:8080
KEY=$(cat ~/.config/locallm/api-key)
curl -s -H "Authorization: Bearer $KEY" $H/v1/models
```

You should see a list with the names of your models. Without the key you'd get `401`.

> 🔐 **Keep the key private.** Don't paste it in chats or commit it to git. To revoke it, delete `keys.txt` on the host and restart the host — a new key is generated, and you copy it to clients again.

### opencode

opencode doesn't know which models the host has, so you list them in its config. Each entry is a model **name exactly as in the host's panel**.

Config file: `~/.config/opencode/opencode.jsonc` (on Windows: `%USERPROFILE%\.config\opencode\opencode.jsonc`).

Example for two models, `qwen-coder` and `gemma` — **replace the address and the model names with yours**:

```jsonc
{
  "$schema": "https://opencode.ai/config.json",
  "model": "gpu/qwen-coder",                         // default model: "gpu/<name>"
  "providers": {
    "gpu": {
      "name": "My GPU PC (locallm)",
      "package": "@opencode/ai/providers/openai-compatible",
      "settings": {
        "baseURL": "http://my-pc.tail1234.ts.net:8080/v1",   // ← host address + /v1
        "apiKey": "{file:~/.config/locallm/api-key}"         // the key is read from the file
      },
      "models": {
        "qwen-coder": {
          "modelID": "qwen-coder",                   // ← must match the name on the host exactly
          "name": "Qwen Coder (GPU PC)",             // any label you like
          "capabilities": { "tools": true, "input": ["text"], "output": ["text"] },
          "limit": { "context": 32768, "output": 8192 }      // context ≤ the model's -c on the host
        },
        "gemma": {
          "modelID": "gemma",
          "name": "Gemma (GPU PC)",
          "capabilities": { "tools": true, "input": ["text", "image"], "output": ["text"] },
          "limit": { "context": 16384, "output": 4096 }
        }
      }
    }
  },
  "agents": { "title": { "disabled": true } }        // ← important, see below
}
```

Things that matter:

| Setting | Why |
|---|---|
| `baseURL` ends with **`/v1`** | opencode talks the OpenAI API, which lives under `/v1`. |
| `modelID` = name on the host | Case matters. `Gemma` ≠ `gemma`. |
| `limit.context` | Not bigger than the `-c` value of that model on the host. If you didn't set `-c`, check the "Context" column in the panel. |
| A **profile** is a separate entry | E.g. `"qwen-coder-long": { "modelID": "qwen-coder-long", … }` with its own bigger `limit.context`. |
| `"agents": {"title": {"disabled": true}}` | Without it opencode sends an extra background request to name each session. On one graphics card that means needless work — or even a model switch. |
| Don't give sub-agents or compaction a *different* local model | Otherwise the host will keep switching models mid-task. |
| `"input": ["text", "image"]` | Only for models that have an `mmproj` file on the host. |

**Using it:** start `opencode` in a project folder. Type `/models` to pick another model — the host switches automatically. The first answer after a switch can take 10–60 s while the model loads; that's normal.

### pi

pi has built-in llama.cpp support, and the host speaks its language. pi can **list, load and unload** models on the host by itself.

**Sign in once.** First copy the key to the clipboard without showing it on screen:

```bash
pbcopy < ~/.config/locallm/api-key          # macOS
# xclip -sel clip < ~/.config/locallm/api-key   # Linux
# Get-Content ~\.config\locallm\api-key | Set-Clipboard   # Windows
```

Then in pi:

```
/login llama.cpp
```

- **URL:** your host address **without** `/v1` — e.g. `http://my-pc.tail1234.ts.net:8080`
- **API key:** paste from the clipboard

<details>
<summary>Alternative: environment variables instead of <code>/login</code></summary>

Add to `~/.zshrc` or `~/.bashrc`:

```bash
export LLAMA_BASE_URL=http://my-pc.tail1234.ts.net:8080
export LLAMA_API_KEY=$(cat ~/.config/locallm/api-key)
```

</details>

**Using it:**

| Command | What it does |
|---|---|
| `/llama` | Shows the host's models with their status. Pick one to load it. If another model is loaded, pi asks what to do — both "unload and load" and "keep and load" end up the same: the host swaps models. |
| `/model` | Choose which **loaded** model to chat with. |

Profiles appear in `/llama` as separate names (e.g. `qwen-coder-long`).

**Thinking levels.** Press **Shift+Tab** in pi to turn thinking on. pi's built-in llama.cpp support offers only two levels, `off` and `medium`, even when the model itself supports more (for example `low`/`medium`/`xhigh`). For an unloaded model the thinking switch may be missing until the model is loaded. This is a pi limitation, not the host's: [pi#10702](https://github.com/earendil-works/pi/issues/10702), still open as of November 2026. Once pi fixes it, the extra levels should appear after updating pi — nothing to change on the host.

| | opencode | pi |
|---|---|---|
| Address | with **`/v1`** | **without** `/v1` |
| Model list | written in the config by hand | fetched from the host automatically |
| Load / unload from the app | by choosing a model | yes, `/llama` |

### Any other app

Anything that supports a custom **OpenAI-compatible** server works (Open WebUI, Continue, Zed, Python `openai` library, …):

| Field | Value |
|---|---|
| Base URL | `http://my-pc.tail1234.ts.net:8080/v1` |
| API key | the contents of `~/.config/locallm/api-key` |
| Model | a name from the host's panel, e.g. `gemma` |

Python example:

```python
from pathlib import Path
from openai import OpenAI

client = OpenAI(
    base_url="http://my-pc.tail1234.ts.net:8080/v1",
    api_key=Path("~/.config/locallm/api-key").expanduser().read_text().strip(),
)
reply = client.chat.completions.create(
    model="gemma",
    messages=[{"role": "user", "content": "Hi! Who are you?"}],
)
print(reply.choices[0].message.content)
```

### Managing the host from a client

**Web panel:** open your host address with `/ui/` at the end, e.g. `http://my-pc.tail1234.ts.net:8080/ui/`. It asks for the **admin key** — a *different* key from the API key, stored on the host in `%LOCALAPPDATA%\locallm\admin-key.txt`. Copy it like the API key (Taildrop + the same block, with `admin-key` instead of `api-key`).

**From the terminal** (with `H` and `KEY` from above):

```bash
# what is loaded right now
curl -s -H "Authorization: Bearer $KEY" $H/api/status

# switch to another model (waits until it's loaded)
curl -s -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d '{"model":"gemma"}' $H/api/swap

# unload and free the graphics card
curl -s -X POST -H "Authorization: Bearer $KEY" $H/api/unload
```

---

## Everyday use

### The tray icon 🦙

The coloured dot in the bottom-right corner of the icon shows the state.

| Dot colour | Meaning |
|---|---|
| ⚪ Grey | No model loaded, graphics card is free |
| 🟡 Yellow | Loading, unloading, or waiting for video memory |
| 🟢 Green | Model ready |
| 🔵 Blue | Paused |

Hover over it to see the model, the number of active requests and VRAM use. Click it for the menu:

| Menu item | Meaning |
|---|---|
| **Load** | Pick a model or profile to load |
| **Unload model (free the GPU)** | Unload the model and free the GPU; answers in progress finish first |
| **Abort requests and unload** | Abort answers in progress and unload right now |
| **Pause** / **Resume** | While paused, clients get a "host paused" error and nothing is loaded. Pause survives restarts |
| **Open panel** | Open the web panel (no key needed on the PC itself) |
| **Open log** / **Open settings folder** | Open the log / the settings folder |
| **Start when signing in to Windows** | Autostart on/off |
| **Language** | English, Русский or 中文; the choice is remembered. The panel opened from the tray uses the same language |
| **Exit** | Unload the model and exit |

### Want to play a game?

| You want | Click |
|---|---|
| Free the GPU, but let clients wake the model again later | **Unload model** |
| Free the GPU and keep it free until you say so | **Pause** (later **Resume**) |
| Free it *right now*, even mid-answer | **Abort requests and unload** |

### Switching models

You don't need to do anything special: whichever model a client asks for gets loaded. Switching takes 10–60 s depending on model size.

One safeguard: for 30 s after a model was last used, a request for a *different* model is refused with `409 model_cooldown` instead of switching. This keeps two people (or two apps) from kicking each other's model out every few seconds. An explicit switch — tray, panel, pi `/llama` or `/api/swap` — always works immediately.

---

## Troubleshooting

<details>
<summary><b>The first answer takes a long time</b></summary>

The model is being loaded into video memory. Big models take up to a minute. Watch the tray icon: yellow → green.
</details>

<details>
<summary><b><code>401 invalid_api_key</code></b></summary>

The key is missing or wrong. Check that `~/.config/locallm/api-key` contains exactly the line from `keys.txt` on the host (43 characters, no spaces). For the web panel you need the **admin** key, not the API key.
</details>

<details>
<summary><b><code>404 model_not_found</code></b></summary>

The app asks for a model name the host doesn't know. Names must match the panel exactly, including case. The error message lists the available names.
</details>

<details>
<summary><b><code>503 host_paused</code></b></summary>

Someone paused the host. Tray → **Resume**, or the same button in the panel.
</details>

<details>
<summary><b><code>503 model_load_failed</code></b></summary>

The model didn't load — most often it doesn't fit into VRAM. Try a smaller `-c` (context) or a smaller model file. Panel → **Log** next to the model shows what llama.cpp said.
</details>

<details>
<summary><b><code>409 model_cooldown</code></b></summary>

Another model was in use less than 30 s ago. Wait the number of seconds in the `Retry-After` header, or switch explicitly (pi `/llama`, the panel, or `/api/swap`).
</details>

<details>
<summary><b>Client can't reach the host at all</b></summary>

1. Is the PC on and the 🦙 icon visible?
2. Are both computers signed into the same Tailscale account? (`tailscale status` on either.)
3. On the host: `tailscale serve status` should show `http://…:8080 → http://127.0.0.1:8080`. If empty, repeat Part 1, Step 6.
4. On the client: enable "Use Tailscale DNS settings".
</details>

<details>
<summary><b>The panel says <code>403 host_not_allowed</code></b></summary>

You opened the panel by a short name or `100.x.y.z` address. Use the full `….ts.net` address, or add that name to `server.admin_hosts` in `%LOCALAPPDATA%\locallm\locallm.yaml` and restart the host.
</details>

<details>
<summary><b>The host doesn't start</b></summary>

It shows a window with the error and the log path. The log is `%LOCALAPPDATA%\locallm\logs\host.log` (tray → **Open log**). Most often it's a typo in a settings file or another program on port 8080.
</details>

---

## Update and uninstall

**Update** — on the host PC:

```powershell
cd $HOME\strigov-locallm
git pull
pwsh -File scripts\install.ps1
```

The script stops the running host, replaces the program and starts it again. Settings, models and keys stay as they are.

**Uninstall:**

```powershell
pwsh -File scripts\uninstall.ps1          # keeps settings and keys
pwsh -File scripts\uninstall.ps1 -Purge   # removes them too
```

It stops the host and removes the program, the shortcut, autostart and the tailnet sharing. llama.cpp, your models and the source folder are never touched.

---

## Security

- The host listens only on `127.0.0.1` — it is not visible on your home network or the internet by itself. Only Tailscale `serve` forwards it, and only inside your tailnet.
- Traffic inside a tailnet is encrypted end‑to‑end by WireGuard, which is why client addresses are plain `http://`.
- Every request except `/health` needs the API key. The panel needs a separate admin key (on the PC itself the tray opens it with a one‑time code).
- Keys, request bodies and answers are never written to the host's log.
- The panel refuses llama.cpp flags that could read or write files, run tools or reach the network. Such flags can only be added by editing `models.yaml` by hand on the PC.

---

## More documentation

- [`docs/reference.ru.md`](docs/reference.ru.md) — full reference (in Russian): all settings, tray and panel items, error codes, exit codes, logs.
- [`locallm.example.yaml`](locallm.example.yaml), [`models.example.yaml`](models.example.yaml) — annotated example settings.
- [`docs/specs/`](docs/specs/) — design specs: how the host works and why (in Russian).
