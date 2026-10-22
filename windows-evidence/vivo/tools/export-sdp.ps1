$ErrorActionPreference='Stop'
$root=(Get-Location).Path
$capture=Get-ChildItem evidence\private -Directory -Filter 'logs-*'|Sort-Object Name -Descending|Select-Object -First 1
$lines=[IO.File]::ReadAllLines((Join-Path $capture.FullName 'rtc\vivoav-2026-10-22.log'))
$script:ipMap=@{};$script:idMap=@{};$script:ssrcMap=@{}
function SafeSdp($marker,$filename) {
 $start=-1
 for($i=0;$i -lt $lines.Length;$i++) {if($lines[$i].Contains($marker)) {$start=$i+1;break}}
 if($start -lt 0) {throw "Missing SDP marker $marker"}
 $sdp=@()
 for($i=$start;$i -lt $lines.Length;$i++) {
  $line=$lines[$i].Trim()
  if($line -match '^\d\d-\d\d \d\d:') {break}
  if($line -notmatch '^[a-z]=') {continue}
  $peer=if($filename.StartsWith('offer')) {'PHONE'}else {'PC'}
  if($line -match '^a=ice-pwd:') {$line="a=ice-pwd:SYNTHETIC_${peer}_PASSWORD_000000"}
  elseif($line -match '^a=ice-ufrag:') {$line="a=ice-ufrag:SYNTHETIC_$peer"}
  elseif($line -match '^a=fingerprint:') {$sdp+=('a=fingerprint:sha-256 '+((1..32|ForEach-Object {'00'}) -join ':'));continue}
  $line=[regex]::Replace($line,'\bufrag\s+\S+',"ufrag SYNTHETIC_$peer")
  $sessionId=if($peer -eq 'PHONE') {'1000000000000000001'}else {'1000000000000000002'}
  $line=[regex]::Replace($line,'(?<=^o=- )\d+',$sessionId)
  $line=[regex]::Replace($line,'(?<=^a=candidate:)\S+',{param($m) if(-not $script:idMap.ContainsKey($m.Value)) {$script:idMap[$m.Value]='foundation'+($script:idMap.Count+1)}; $script:idMap[$m.Value]})
  $line=[regex]::Replace($line,'\b(?:\d{1,3}\.){3}\d{1,3}\b|\b(?:[a-fA-F0-9]{0,4}:){2,}[a-fA-F0-9:]+',{param($m)
   if($m.Value -in @('0.0.0.0','127.0.0.1')) {return $m.Value}
   if(-not $script:ipMap.ContainsKey($m.Value)) {if($m.Value.Contains(':')) {$script:ipMap[$m.Value]='2001:db8::'+($script:ipMap.Count+1)}else {$script:ipMap[$m.Value]='192.0.2.'+($script:ipMap.Count+1)}}
   $script:ipMap[$m.Value]
  })
  if($line -match '^a=ssrc:|^a=ssrc-group:') {
   $line=[regex]::Replace($line,'\b\d{6,10}\b',{param($m) if(-not $script:ssrcMap.ContainsKey($m.Value)) {$script:ssrcMap[$m.Value]=[string](100000001+$script:ssrcMap.Count)}; $script:ssrcMap[$m.Value]})
   $line=$line -replace 'cname:\S+','cname:SYNTHETIC_CNAME'
  }
  $sdp+=$line
 }
 [IO.File]::WriteAllText((Join-Path $root $filename),($sdp -join "`r`n")+"`r`n")
 [pscustomobject]@{file=$filename;lines=$sdp.Count;media=@($sdp|Where-Object {$_ -match '^m='});marker=$marker;logLine=$start}
}
SafeSdp 'Decrypted sdp:' 'offer-redacted.sdp'
SafeSdp 'OnSuccess SetLocalDescription, Local Sdp:' 'answer-redacted.sdp'
SafeSdp 'Updated Local Sdp:' 'answer-gathered-redacted.sdp'
