$ErrorActionPreference = 'Stop'
$exe = 'D:\WorkBuddy\wb2api.exe'
$wd  = 'D:\WorkBuddy'
$out = 'D:\WorkBuddy\data\wb2api.out.log'
$err = 'D:\WorkBuddy\data\wb2api.err.log'

# 只保留最近 2 MiB，免得日志无限长
foreach ($f in @($out, $err)) {
  if (Test-Path $f) {
    $len = (Get-Item $f).Length
    if ($len -gt 2MB) {
      $tail = Get-Content $f -Tail 2000 -Encoding UTF8
      [IO.File]::WriteAllLines($f, $tail, (New-Object Text.UTF8Encoding $false))
    }
  }
}

$psi = New-Object Diagnostics.ProcessStartInfo
$psi.FileName = 'cmd.exe'
$psi.Arguments = '/c ""' + $exe + '" -config config.json >> "' + $out + '" 2>> "' + $err + '""'
$psi.WorkingDirectory = $wd
$psi.UseShellExecute = $false
$psi.CreateNoWindow = $true
$p = [Diagnostics.Process]::Start($psi)
"launcher pid=$($p.Id)  -> $exe"
Start-Sleep -Seconds 6
Get-Process wb2api -ErrorAction SilentlyContinue |
  Select-Object Id, StartTime, @{n='MB';e={[math]::Round($_.WorkingSet64/1MB,1)}} |
  Format-Table -AutoSize | Out-String
