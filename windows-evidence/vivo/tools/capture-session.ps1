$ErrorActionPreference='Stop'
$root='C:\Users\rubber pirate\vivo'
$capture=Join-Path $root ('evidence\private\capture-'+[DateTime]::UtcNow.ToString('yyyyMMddTHHmmssZ'))
New-Item -ItemType Directory -Path $capture -Force | Out-Null
$logRoot=Join-Path $env:APPDATA 'pcsuite\log\vivocontrol'
$script:seen=@{}
$script:tick=0
function Mark([string]$stage,$detail) {
 $record=[ordered]@{timestamp=[DateTimeOffset]::Now.ToString('o');source='capture-harness';stage=$stage;detail=$detail}
 $record|ConvertTo-Json -Compress -Depth 8|Add-Content -Encoding UTF8 (Join-Path $root 'timeline.jsonl')
}
function Snapshot {
 foreach($file in Get-ChildItem -LiteralPath $logRoot -Recurse -File -ErrorAction SilentlyContinue) {
  if($file.Extension -ne '.log') {continue}
  $key=$file.FullName; $sig="$($file.Length):$($file.LastWriteTimeUtc.Ticks)"
  if($script:seen[$key] -eq $sig) {continue}
  $relative=$key.Substring($logRoot.Length).TrimStart('\')
  $target=Join-Path $capture $relative
  New-Item -ItemType Directory -Path (Split-Path $target -Parent) -Force|Out-Null
  try {
   $inputStream=[IO.File]::Open($key,[IO.FileMode]::Open,[IO.FileAccess]::Read,[IO.FileShare]::ReadWrite)
   $outputStream=[IO.File]::Create($target)
   try {$inputStream.CopyTo($outputStream)} finally {$inputStream.Dispose();$outputStream.Dispose()}
   $script:seen[$key]=$sig
   [ordered]@{timestamp=[DateTimeOffset]::Now.ToString('o');file=$relative;bytes=$file.Length;lastWriteUtc=$file.LastWriteTimeUtc.ToString('o')}|ConvertTo-Json -Compress|Add-Content -Encoding UTF8 (Join-Path $capture 'file-observations.jsonl')
  } catch { Mark 'snapshot-error' @{file=$relative;error=$_.Exception.GetType().Name} }
 }
}
Add-Type -AssemblyName System.Windows.Forms,System.Drawing
$form=New-Object Windows.Forms.Form
$form.Text='Remote PC harmless input and video test'
$form.Size=New-Object Drawing.Size(800,620)
$form.StartPosition='CenterScreen'
$label=New-Object Windows.Forms.Label
$label.Location=New-Object Drawing.Point(15,15);$label.Size=New-Object Drawing.Size(750,55)
$label.Font=New-Object Drawing.Font('Consolas',16)
$form.Controls.Add($label)
$box=New-Object Windows.Forms.TextBox
$box.Multiline=$true;$box.ScrollBars='Vertical';$box.Location=New-Object Drawing.Point(15,90);$box.Size=New-Object Drawing.Size(750,400)
$box.Text="Type REMOTE TEST here. Test Ctrl+A, scrolling, left/right clicks, and drag this window.`r`n"+((1..70|ForEach-Object {"Harmless scrolling line $_"}) -join "`r`n")
$form.Controls.Add($box)
$button=New-Object Windows.Forms.Button
$button.Text='Mark: phone desktop visible';$button.Location=New-Object Drawing.Point(15,510);$button.Size=New-Object Drawing.Size(260,40)
$button.Add_Click({Mark 'phone-desktop-visible-operator-marker' @{precision='manual marker; not exact display time'}})
$form.Controls.Add($button)
$box.Add_MouseDown({param($sender,$event) Mark 'test-window-mouse-down' @{button=$event.Button.ToString();x=$event.X;y=$event.Y}})
$box.Add_MouseUp({param($sender,$event) Mark 'test-window-mouse-up' @{button=$event.Button.ToString();x=$event.X;y=$event.Y}})
$box.Add_MouseWheel({param($sender,$event) Mark 'test-window-scroll' @{delta=$event.Delta}})
$box.Add_KeyDown({param($sender,$event) Mark 'test-window-key-down' @{keyCode=[int]$event.KeyCode;control=$event.Control;alt=$event.Alt;shift=$event.Shift}})
$form.Add_Move({Mark 'test-window-moved' @{x=$form.Left;y=$form.Top}})
$timer=New-Object Windows.Forms.Timer
$timer.Interval=1000
$timer.Add_Tick({
 $script:tick++
 $label.Text="REMOTE TEST tick $script:tick`r`n"+[DateTimeOffset]::Now.ToString('yyyy-MM-dd HH:mm:ss.fff zzz')
 if($script:tick % 2 -eq 0) {Snapshot}
})
$form.Add_FormClosed({$timer.Stop();Snapshot;Mark 'capture-stopped' @{ticks=$script:tick}})
Mark 'capture-started' @{privateEvidence=$capture;mode='existing vendor log snapshots; no packet or runtime instrumentation'}
Snapshot
$timer.Start()
[Windows.Forms.Application]::Run($form)
