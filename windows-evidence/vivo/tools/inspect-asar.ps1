param([string]$Archive='C:\Program Files (x86)\pcsuite\vivoControl\resources\app.asar')
$ErrorActionPreference='Stop'
$root=if($PSScriptRoot) { Split-Path $PSScriptRoot -Parent } else { (Get-Location).Path }
$out=Join-Path $root 'evidence\private\source'
New-Item -ItemType Directory -Path $out -Force | Out-Null
$stream=[IO.File]::OpenRead($Archive)
$reader=New-Object IO.BinaryReader($stream)
$null=$reader.ReadUInt32(); $headerSize=$reader.ReadUInt32(); $null=$reader.ReadUInt32(); $jsonSize=$reader.ReadUInt32()
$tree=([Text.Encoding]::UTF8.GetString($reader.ReadBytes($jsonSize)))|ConvertFrom-Json
function Walk($node,$prefix) {
 foreach($entry in $node.files.PSObject.Properties) {
  $name="$prefix/$($entry.Name)"
  if($entry.Value.files) { Walk $entry.Value $name }
  elseif($name -notmatch '/node_modules/' -and $name -match '\.(js|json)$' -and $name -notmatch '/assets/') {
   if(-not $entry.Value.unpacked) {
    $null=$stream.Seek(8+$headerSize+[long]$entry.Value.offset,[IO.SeekOrigin]::Begin)
    $bytes=$reader.ReadBytes([int]$entry.Value.size)
    $target=Join-Path $out ($name.TrimStart('/') -replace '/','_')
    [IO.File]::WriteAllBytes($target,$bytes)
   }
  }
 }
}
try { Walk $tree '' } finally { $reader.Close() }
$terms='deviceScreenInfo','getDeviceScreenInfo','deviceScreenDpiInfo','accountList','accountAuthResult','clientConnectSuccess','vivodatachannel','createDataChannel','0xFE','0xfe','confirm','keyFrame','keyframe','screenLock','sctp'
$result=@()
foreach($file in Get-ChildItem $out -Filter '*.js') {
 $content=[IO.File]::ReadAllText($file.FullName)
 foreach($term in $terms) {
  $pos=0; $count=0
  while(($pos=$content.IndexOf($term,$pos,[StringComparison]::OrdinalIgnoreCase)) -ge 0 -and $count -lt 20) {
   $start=[Math]::Max(0,$pos-500); $len=[Math]::Min(1700,$content.Length-$start)
   $result += [pscustomobject]@{File=$file.Name;Term=$term;Offset=$pos;Context=$content.Substring($start,$len)}
   $pos+=$term.Length; $count++
  }
 }
}
$result | ConvertTo-Json -Depth 5 | Set-Content -Encoding UTF8 (Join-Path $root 'evidence\private\static-contexts.json')
$result | Group-Object Term | Select-Object Name,Count | ConvertTo-Json -Compress
