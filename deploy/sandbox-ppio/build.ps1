$ErrorActionPreference = 'Stop'
$node = (Get-Command node -CommandType Application | Select-Object -First 1).Source

if ($args -contains '--help') {
    & $node "$PSScriptRoot/build.mjs" @args
    exit $LASTEXITCODE
}

$builderRoot = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\', '/')
$builderDirectory = Join-Path $builderRoot ("cloudpilot-template-" + [guid]::NewGuid())
$previousNodePath = $env:NODE_PATH
$previousProxy = $env:NODE_USE_ENV_PROXY
$buildExitCode = 1

try {
    New-Item -ItemType Directory -Path $builderDirectory | Out-Null
    $npmCLI = Join-Path (Split-Path $node) 'node_modules/npm/bin/npm-cli.js'
    if (-not (Test-Path -LiteralPath $npmCLI -PathType Leaf)) {
        throw 'Install Node.js with its bundled npm before building the template'
    }
    $env:NODE_USE_ENV_PROXY = '1'
    & $node $npmCLI install --prefix $builderDirectory --cache "$builderDirectory/npm-cache" --no-audit --no-fund novita-sandbox@2.1.2 |
        ForEach-Object { [Console]::Error.WriteLine($_) }
    if ($LASTEXITCODE -ne 0) { throw 'Failed to install the Novita build SDK' }
    $env:NODE_PATH = (Join-Path $builderDirectory 'node_modules') + [IO.Path]::PathSeparator + $previousNodePath
    & $node "$PSScriptRoot/build.mjs" @args
    $buildExitCode = $LASTEXITCODE
} finally {
    $env:NODE_PATH = $previousNodePath
    $env:NODE_USE_ENV_PROXY = $previousProxy
    if (Test-Path -LiteralPath $builderDirectory) {
        if ((Split-Path (Resolve-Path -LiteralPath $builderDirectory).Path -Parent) -ne $builderRoot) {
            throw 'Refusing to remove a build directory outside the temporary directory'
        }
        Remove-Item -LiteralPath $builderDirectory -Recurse -Force
    }
}
exit $buildExitCode
