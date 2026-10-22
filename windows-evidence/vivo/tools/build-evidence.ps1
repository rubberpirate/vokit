$ErrorActionPreference='Stop'
$root=(Get-Location).Path
$capture=Get-ChildItem evidence\private -Directory -Filter 'logs-*'|Sort-Object Name -Descending|Select-Object -First 1
$out=Join-Path $root 'evidence\sanitized'
New-Item -ItemType Directory -Path $out -Force|Out-Null
$events=New-Object 'System.Collections.Generic.List[object]'
function Event($timestamp,$stage,$source,$line,$detail) {
 $events.Add([pscustomobject][ordered]@{timestamp=$timestamp;clock='PC local log clock';stage=$stage;source=$source;line=$line;detail=$detail})
}
foreach($relative in @('service\service-2026-10-22.log','rtc\vivoav-2026-10-22.log','rtc\exosip-2026-10-22.log')) {
 $lines=[IO.File]::ReadAllLines((Join-Path $capture.FullName $relative))
 $excerpt=New-Object 'System.Collections.Generic.List[string]'
 $lastTime=$null
 for($i=0;$i -lt $lines.Length;$i++) {
  $line=$lines[$i]
  if($line -match '^(2026-\d\d-\d\d \d\d:\d\d:\d\d\.\d{3})') {$lastTime=$Matches[1] -replace ' ','T';$lastTime+='+05:30'}
  elseif($line -match '^(\d\d-\d\d) (\d\d:\d\d:\d\d):(\d{3})') {$lastTime='2026-'+$Matches[1]+'T'+$Matches[2]+'.'+$Matches[3]+'+05:30'}
  $session=if($lastTime -ge '2026-10-22T15:16:56') {'CONNECTION_2'}elseif($lastTime -ge '2026-10-22T15:09:23') {'CONNECTION_1'}else {'PRE_SESSION'}
  $screenMessage=if($session -eq 'CONNECTION_2') {'SCREEN_MSG_2'}else {'SCREEN_MSG_1'}
  $stage=$null;$detail=@{}
  if($relative.StartsWith('service')) {
   if($line.Contains('action:open in websocket')) {$stage='cloud-open';$detail=@{direction='phone -> PC via cloud';transport='WebSocket';connectionType=0;ability=@{cursor=1;dcHeartBeat=1;needAccountAuth=1;p2pSecurity=1;passwordType=1};identifiers='PHONE, PC, CONNECTION_1';payloadExample='synthetic identifiers; actual secrets omitted'}}
   elseif($line.Contains('initParams in sendToAndroidScreenLock')) {$stage='screenLockInfo-send';$detail=@{transport='WebSocket';direction='PC -> phone';param=@{type='screenLockInfo';isLock=$false;passwordInputPosition='<value omitted>'}}}
   elseif($line.Contains('发送opensuccess')) {$stage='cloud-openSuccess-send';$detail=@{transport='WebSocket';direction='PC -> phone';fieldsFromStaticCode=@('commandTransport=datachannel','dcHeartBeat=1','p2pSecurityInfo=<REDACTED>')}}
   elseif($line.Contains('action confirm') -and $line.Contains('deviceScreenInfo')) {$stage='deviceScreenInfo-confirm';$detail=@{transport='WebSocket';direction='phone -> PC';action='confirm';data=@{type='deviceScreenInfo';name=''};msgId=$screenMessage;connectionId=$session;sourceDeviceNo='PHONE';targetDeviceNo='PC'}}
   elseif($line -match 'eventType:(AC\d+)-eventDesc:(.*)$') {
    $code=$Matches[1];$desc=$Matches[2]
    $stage=switch($code){'AC10009'{'first-frame-captured'}'AC10004'{'sip-invite-service-callback'}'AC10013'{'sip-invite-accepted-service'}'AC10103'{'sdp-create-service-callback'}'AC10104'{'sdp-created-service-callback'}'AC10007'{'transport-p2p-service-callback'}'AC10014'{'sip-answer-success-service-callback'}'AC10016'{'data-channel-connected-service-callback'}'AC10010'{'first-frame-sent-service-callback'}default{$null}}
    if($stage) {$detail=@{vendorEvent=$code};$excerpt.Add("$($i+1): $lastTime $code $stage")}
   }
  }elseif($relative.Contains('vivoav')) {
   if($line.Contains('Decrypted sdp:')) {$stage='offer-decrypted';$detail=@{artifact='offer-redacted.sdp'}}
   elseif($line.Contains('OnSuccess SetLocalDescription, Local Sdp:')) {$stage='local-answer-before-encryption';$detail=@{artifact='answer-redacted.sdp'}}
   elseif($line.Contains('Updated Local Sdp:')) {$stage='local-answer-with-candidates';$detail=@{artifact='answer-gathered-redacted.sdp'}}
   elseif($line.Contains('New selected connection:')) {$stage='ice-selected-pair';$detail=@{localType='host';remoteType='host';transport='UDP';addressFamily='IPv6';relaySelected=$false}}
   elseif($line.Contains('DTLS handshake complete.')) {$stage='dtls-established'}
   elseif($line.Contains('datachannel OnStateChange1')) {$stage='data-channel-open';$detail=@{state=1;sid=100;labelFromBinaryStrings='vivodatachannel'}}
   elseif($line.Contains('datachannel sctp OnStateChange state:2')) {$stage='sctp-connected';$detail=@{state=2}}
   elseif($line -match 'recv msg type: (254|255) size: (\d+)') {
    $stage='data-channel-heartbeat-received';$detail=@{direction='phone -> PC';type=[int]$Matches[1];size=[int]$Matches[2];payloadBytes='not logged'}
   }
   elseif($line.Contains('Send key frame, packet_queue bytes')) {$stage='video-keyframe-paced';$detail=@{codec='H264';payloadType=98;nalUnits='not logged'}}
   elseif($line -match 'mouse action: (\w+)') {$stage='input-mouse-injection';$detail=@{action=$Matches[1];transport='data channel, inferred from code';coordinates='omitted'};$excerpt.Add("$($i+1): $lastTime mouse action=$($Matches[1]); coordinates omitted")}
   elseif($line -match 'input_event_simulator.*key') {$stage='input-keyboard-injection';$detail=@{keyPayload='omitted'};$excerpt.Add("$($i+1): $lastTime keyboard injection; key payload omitted")}
   if($stage -and $stage -notmatch 'heartbeat|ice-selected|input-') {$excerpt.Add("$($i+1): $line")}
  }elseif($relative.Contains('exosip')) {
   if($line -match '^(INVITE|ACK|BYE) sip:') {$stage='sip-'+$Matches[1].ToLower();$detail=@{requestLine=($Matches[1]+' sip:PEER@SIP_HOST SIP/2.0')};$excerpt.Add("$($i+1): preceding-log-time=$lastTime $($detail.requestLine)")}
   elseif($line -match '^SIP/2.0 (100|180|200) (.*)') {$stage='sip-response';$detail=@{status=[int]$Matches[1];reason=$Matches[2];caution='Includes REGISTER/OPTIONS responses; correlate with adjacent request/CSeq in private evidence'};$excerpt.Add("$($i+1): preceding-log-time=$lastTime $line")}
  }
  if($stage) {
   $session=if($lastTime -ge '2026-10-22T15:16:56') {'CONNECTION_2'}elseif($lastTime -ge '2026-10-22T15:09:23') {'CONNECTION_1'}else {'PRE_SESSION'}
   if($detail.ContainsKey('connectionId')) {$detail.connectionId=$session}
   if($detail.ContainsKey('identifiers')) {$detail.identifiers="PHONE, PC, $session"}
   $detail['session']=$session
   Event $lastTime $stage $relative ($i+1) $detail
  }
  if($relative.StartsWith('service') -and $stage -and $stage -notmatch 'service-callback|frame-captured') {$excerpt.Add("$($i+1): $lastTime $stage "+($detail|ConvertTo-Json -Compress -Depth 6))}
 }
 [IO.File]::WriteAllLines((Join-Path $out ($relative -replace '\\','_')),$excerpt)
}
$events|Sort-Object timestamp|ForEach-Object {$_|ConvertTo-Json -Compress -Depth 8}|Set-Content -Encoding UTF8 (Join-Path $root 'timeline.jsonl')
$events|Group-Object stage|Select-Object Name,Count|ConvertTo-Json -Compress
