# The call arguments arrive as one JSON object on stdin.
$args_ = [Console]::In.ReadToEnd() | ConvertFrom-Json
@{ output = "Hello, $($args_.name)! (from PowerShell, workspace: $env:ZAGENT_WORKSPACE)" } | ConvertTo-Json -Compress
