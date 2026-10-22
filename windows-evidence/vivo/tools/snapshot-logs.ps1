param([int]$Seconds=0)
$ErrorActionPreference='Stop'
$root=(Get-Location).Path
$source='C:\Users\rubber pirate\AppData\Roaming\pcsuite\log\vivocontrol'
$capture=Join-Path $root ('evidence\private\logs-'+[DateTime]::UtcNow.ToString('yyyyMMddTHHmmssZ'))
New-Item -ItemType Directory -Path $capture -Force|Out-Null
$end=[DateTime]::UtcNow.AddSeconds($Seconds)
$seen=@{}
do {
 foreach($f in Get-ChildItem -LiteralPath $source -Recurse -File -Filter '*.log') {
  $sig="$($f.Length):$($f.LastWriteTimeUtc.Ticks)"
  if($seen[$f.FullName] -eq $sig) {continue}
  $relative=$f.FullName.Substring($source.Length).TrimStart('\')
  $dest=Join-Path $capture $relative
  New-Item -ItemType Directory -Path (Split-Path $dest -Parent) -Force|Out-Null
  $inputStream=[IO.File]::Open($f.FullName,[IO.FileMode]::Open,[IO.FileAccess]::Read,[IO.FileShare]::ReadWrite)
  $outputStream=[IO.File]::Create($dest)
  try {$inputStream.CopyTo($outputStream)} finally {$inputStream.Dispose();$outputStream.Dispose()}
  $seen[$f.FullName]=$sig
  [ordered]@{timestamp=[DateTimeOffset]::Now.ToString('o');file=$relative;bytes=$f.Length;sha256=(Get-FileHash -LiteralPath $dest -Algorithm SHA256).Hash} | ConvertTo-Json -Compress | Add-Content -Encoding UTF8 (Join-Path $capture 'manifest.jsonl')
 }
 if($Seconds -gt 0) {Start-Sleep -Seconds 2}
}while([DateTime]::UtcNow -lt $end)
Write-Output $capture
