# OpenConsole client installer for Windows.
#
#   irm https://openconsole.dev/install.ps1 | iex
#
# Installs the `openconsole` client, which shares a terminal or joins one.
# It does not install or configure a relay.
#
# You do not need this to *join* a shared terminal: a browser works with
# nothing installed, and so does `ssh <session>@<relay>`. This is for sharing
# your own console — cmd.exe by default, PowerShell with `-shell powershell.exe`
# — or for joining from one.
#
# Nothing here needs administrator rights: the client goes in a per-user
# directory, and only that user's PATH is touched.

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$Repo = 'SmugZombie/OpenConsole'
$Bin  = 'openconsole.exe'

# Where to install, which version, and where releases come from. All three are
# overridable, so this can be pointed at a mirror on a network that cannot
# reach GitHub — and so the script itself can be tested against a local server
# rather than only in production.
$Prefix  = if ($env:OPENCONSOLE_PREFIX)   { $env:OPENCONSOLE_PREFIX }   else { Join-Path $env:LOCALAPPDATA 'Programs\OpenConsole' }
$Version = if ($env:OPENCONSOLE_VERSION)  { $env:OPENCONSOLE_VERSION }  else { 'latest' }
$BaseUrl = if ($env:OPENCONSOLE_BASE_URL) { $env:OPENCONSOLE_BASE_URL } else { "https://github.com/$Repo/releases" }

function Say  { param([string]$Message) Write-Host $Message }
function Warn { param([string]$Message) Write-Host $Message -ForegroundColor Yellow }
function Die  { param([string]$Message) throw $Message }

# Get-Platform names the release archive for this machine.
function Get-Platform {
    # PROCESSOR_ARCHITECTURE describes the *process*, so a 32-bit PowerShell on
    # a 64-bit machine reports x86 and puts the real answer in ARCHITEW6432.
    $arch = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
    switch ($arch) {
        'AMD64' { return 'windows_amd64' }
        'ARM64' { return 'windows_arm64' }
        default { Die "no prebuilt binary for $arch. Build from source: https://github.com/$Repo" }
    }
}

# Test-ConPTY reports whether this Windows can host a shared console at all.
#
# Sharing needs a pseudo-console, which arrived in Windows 10 1809. Joining
# works on anything, so this is a warning rather than a refusal: someone may be
# installing the client purely to join.
function Test-ConPTY {
    # Read the build from the registry rather than [Environment]::OSVersion,
    # which is subject to application compatibility shims and can report
    # Windows 8 on a Windows 11 machine. An unreadable key means no opinion.
    $build = 0
    try {
        $key = Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion' -Name CurrentBuildNumber
        $build = [int]$key.CurrentBuildNumber
    } catch { }

    if ($build -gt 0 -and $build -lt 17763) {
        Warn "This is Windows build $build. Sharing a console needs 1809 (build 17763) or newer;"
        Warn "joining a session shared by someone else will still work."
    }
}

# Get-Release downloads the release archive and returns the extracted binary.
function Get-Release {
    param([string]$Platform, [string]$Work)

    $url = if ($Version -eq 'latest') {
        "$BaseUrl/latest/download/openconsole_$Platform.zip"
    } else {
        "$BaseUrl/download/$Version/openconsole_$Platform.zip"
    }

    Say "Downloading openconsole ($Platform)..."
    $archive = Join-Path $Work 'openconsole.zip'
    # The progress bar makes Invoke-WebRequest an order of magnitude slower on
    # Windows PowerShell, and nobody is watching a two-second download.
    $progress = $ProgressPreference
    $ProgressPreference = 'SilentlyContinue'
    try {
        # Old hosts default to TLS 1.0, which GitHub refuses.
        try { [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12 } catch { }
        Invoke-WebRequest -Uri $url -OutFile $archive -UseBasicParsing
    } catch {
        Warn "Could not download $url"
        return $null
    } finally {
        $ProgressPreference = $progress
    }

    Expand-Archive -Path $archive -DestinationPath $Work -Force
    $exe = Join-Path $Work $Bin
    if (-not (Test-Path $exe)) { Die "the downloaded archive did not contain $Bin" }
    return $exe
}

# Install-FromSource builds with Go when no release matches, so the installer is
# still useful on a platform we do not ship binaries for.
function Install-FromSource {
    param([string]$Destination)

    if (-not (Get-Command go -ErrorAction SilentlyContinue)) { return $false }
    Warn 'No release binary available; building from source with Go...'
    $env:GOBIN = $Destination
    # Out-Host, not a bare call: a native command writes to the success stream,
    # and anything left there would be returned from this function alongside
    # the result, turning a failed build into a truthy answer.
    & go install "github.com/$Repo/cmd/openconsole@latest" | Out-Host
    return ($LASTEXITCODE -eq 0)
}

# Add-ToPath puts the install directory on the user's PATH when it is missing,
# which is the single most common reason a fresh install "does not work".
function Add-ToPath {
    param([string]$Directory)

    $user = [Environment]::GetEnvironmentVariable('Path', 'User')
    $entries = @()
    if ($user) { $entries = $user -split ';' | Where-Object { $_ } }
    if ($entries -contains $Directory) { return }

    [Environment]::SetEnvironmentVariable('Path', (@($entries) + $Directory) -join ';', 'User')
    # The change lands in the registry, and only new processes read it. Patch
    # this session too so `openconsole` works in the window it was installed in.
    $env:Path = "$env:Path;$Directory"
    Say ''
    Say "Added $Directory to your PATH."
    Say 'Open a new terminal for other windows to see it.'
}

function Install-OpenConsole {
    Test-ConPTY
    $platform = Get-Platform
    New-Item -ItemType Directory -Force -Path $Prefix | Out-Null

    $work = Join-Path ([System.IO.Path]::GetTempPath()) ("openconsole-" + [Guid]::NewGuid().ToString('n'))
    New-Item -ItemType Directory -Force -Path $work | Out-Null
    try {
        $target = Join-Path $Prefix $Bin
        $exe = Get-Release -Platform $platform -Work $work
        if ($exe) {
            try {
                Move-Item -Path $exe -Destination $target -Force
            } catch {
                Die "could not write $target. Close any running openconsole, or set OPENCONSOLE_PREFIX to somewhere you can write."
            }
        } elseif (-not (Install-FromSource -Destination $Prefix)) {
            Die @"
no release binary for this platform, and building from source did not work.

This usually means one of:
  * no release has been published yet;
  * the repository is private, so neither the release download nor
    ``go install`` can reach it without credentials;
  * this machine cannot reach $BaseUrl.

Ask whoever runs the relay for a binary, or build one from a checkout:
  go build -o $Bin ./cmd/openconsole
"@
        }

        # Never announce a binary that is not there. Every path to "Installed"
        # goes through this check.
        if (-not (Test-Path $target)) { Die "installation did not produce $target" }

        Say "Installed $target"
        & $target version
        Add-ToPath -Directory $Prefix
    } finally {
        Remove-Item -Recurse -Force $work -ErrorAction SilentlyContinue
    }
}

try {
    Install-OpenConsole
} catch {
    Write-Host "install: $($_.Exception.Message)" -ForegroundColor Red
}
