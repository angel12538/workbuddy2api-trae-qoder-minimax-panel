' ===========================================================================
'  wb2api_run.vbs
'  Silent launcher for the WorkBuddy2API Panel server (wb2api.exe).
'
'  Why VBS: wscript.exe is a GUI-subsystem process, so it NEVER allocates a
'  console window. It launches cmd.exe with window style 0 (SW_HIDE), so
'  nothing flashes on screen while the user works in another app.
'
'  ⚠️ Two traps this script exists to dodge:
'
'  1) WScript.Shell.Run does NOT go through a command interpreter on its own:
'     you must name cmd.exe explicitly, otherwise "/c" and the ">>" redirection
'     are handed to the target program as literal arguments.
'
'  2) cmd.exe's /C quote handling: when the string after /C contains more
'     than two quote characters, cmd strips the FIRST and the LAST quote on
'     the command line. So the whole argument must end with TWO quotes
'     ("path"") or the last path loses its closing quote and cmd bails out
'     with exit code 1 and a confusing "incorrect syntax" error.
'     The quotes are built with Chr(34) below so the count is unambiguous.
'
'  Why it WAITS (bWaitOnReturn = True):
'  the scheduled task "WorkBuddy2API" uses MultipleInstances = IgnoreNew.
'  The scheduler decides "an instance is already running" purely by whether
'  the task's action process is still alive. If this VBS launched the server
'  and exited immediately, the task instance would end right away, the next
'  5-minute repetition tick would start a SECOND server, and the second one
'  would fail to bind :7863. Waiting keeps the task instance alive for
'  exactly as long as the server runs, so IgnoreNew does its job.
'
'  The 5-minute repetition trigger on the task then doubles as a watchdog:
'  if wb2api.exe dies, the next tick restarts it with no human involved.
'  That matters because CodeArts / Huawei Cloud sessions are idle-timeout
'  based: they only survive while the server keeps polling.
'
'  Log: D:\WorkBuddy\data\wb2api.out.log / wb2api.err.log
' ===========================================================================
Option Explicit

' global error trapping on purpose: an unhandled WSH error pops a MODAL
' dialog that blocks the script forever and leaves the scheduled task stuck
' in "Running" with no server behind it.
On Error Resume Next

Dim shell, fso, exe, wd, outLog, errLog, cmd, exitCode, Q, vbsLog

Set shell = CreateObject("WScript.Shell")
Set fso = CreateObject("Scripting.FileSystemObject")

Q      = Chr(34)
exe    = "D:\WorkBuddy\wb2api.exe"
wd     = "D:\WorkBuddy"
outLog = wd & "\data\wb2api.out.log"
errLog = wd & "\data\wb2api.err.log"
vbsLog = wd & "\data\wb2api.vbs.log"

If Not fso.FolderExists(wd & "\data") Then
    fso.CreateFolder(wd & "\data")
End If

If Not fso.FileExists(exe) Then
    fso.CreateTextFile(vbsLog, True).WriteLine _
        "[wb2api_run] FAIL: missing " & exe & " at " & Now
    WScript.Quit 3
End If

shell.CurrentDirectory = wd

' cmd.exe /c ""<exe>" -config config.json >> "<out>" 2>> "<err>""
' NOTE the trailing Q & Q: cmd strips the first and last quote of the /C
' argument, so the argument itself must start and end with a quote pair.
cmd = "cmd.exe /c " & Q & Q & exe & Q & _
      " -config config.json >> " & Q & outLog & Q & _
      " 2>> " & Q & errLog & Q & Q

' window style 0 = SW_HIDE (no console flash), True = wait for completion
exitCode = shell.Run(cmd, 0, True)

If Err.Number <> 0 Then
    fso.CreateTextFile(vbsLog, True).WriteLine _
        "[wb2api_run] FAIL: Run error " & Err.Number & " " & Err.Description & " at " & Now
    WScript.Quit 9
End If

fso.CreateTextFile(vbsLog, True).WriteLine _
    "[wb2api_run] server exited with code " & exitCode & " at " & Now

WScript.Quit exitCode
