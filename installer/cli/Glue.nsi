Unicode true

!include "MUI2.nsh"
!include "LogicLib.nsh"
!include "FileFunc.nsh"
!include "nsDialogs.nsh"

!ifndef PAYLOAD_VERSION
  !define PAYLOAD_VERSION "0.1.0"
!endif
!ifndef PAYLOAD_ARCH
  !define PAYLOAD_ARCH "amd64"
!endif

!define PRODUCT_NAME "Gluestick CLI"
!define UNINSTALL_KEY "Software\Microsoft\Windows\CurrentVersion\Uninstall\Gluestick CLI ${PAYLOAD_ARCH}"
!define PAYLOAD_DIR "payload\${PAYLOAD_ARCH}"
!define INSTALL_LOG "install.log"

; Vibe Coding packages shown on the selection page.
!define VIBE_CLAUDE "claude-code"
!define VIBE_CODEX "codex"
!define VIBE_MAGPIE "extras/magpie-ai"

Var VibeClaude
Var VibeCodex
Var VibeMagpie
Var VibeClaudeCtl
Var VibeCodexCtl
Var VibeMagpieCtl

; Free-space floors in megabytes. The CLI figure covers glue.exe, shim.exe and
; the workspace metadata with some headroom; the template figure adds room for
; claude-code, codex, Magpie AI, the helper tools and the content store.
!ifndef MIN_CLI_MB
  !define MIN_CLI_MB 64
!endif
!ifndef MIN_TEMPLATE_MB
  !define MIN_TEMPLATE_MB 1024
!endif

Name "${PRODUCT_NAME} ${PAYLOAD_VERSION} (${PAYLOAD_ARCH})"
!ifndef OUT_FILE
  !define OUT_FILE "output\GluestickSetup-${PAYLOAD_ARCH}.exe"
!endif
OutFile "${OUT_FILE}"
InstallDir "$LOCALAPPDATA\Programs\Gluestick CLI"
InstallDirRegKey HKCU "${UNINSTALL_KEY}" "InstallLocation"
RequestExecutionLevel user
ShowInstDetails show
ShowUninstDetails show
SetCompressor /SOLID lzma

!define MUI_ABORTWARNING
!define MUI_FINISHPAGE_TEXT "Gluestick is installed.$\r$\n$\r$\nInstallation log: $INSTDIR\${INSTALL_LOG}"
!define MUI_FINISHPAGE_SHOWREADME ""
!define MUI_FINISHPAGE_SHOWREADME_TEXT "Show installation log"
!define MUI_FINISHPAGE_SHOWREADME_FUNCTION ShowInstallLog
!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_COMPONENTS
Page custom VibePageCreate VibePageLeave
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH
!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES
!insertmacro MUI_LANGUAGE "English"

Section "Gluestick CLI" SecMain
  SectionIn RO
  SetOutPath "$INSTDIR"
  ; Create/truncate the install log in the installation root. NSIS's built-in
  ; logging is not compiled into every makensis build, so this writes the file
  ; explicitly and `glue tee` appends the CLI output below.
  FileOpen $9 "$INSTDIR\${INSTALL_LOG}" w
  FileWrite $9 "Gluestick ${PAYLOAD_VERSION} (${PAYLOAD_ARCH}) installation$\r$\n"
  FileWrite $9 "Install directory: $INSTDIR$\r$\n"
  FileClose $9
  Call CheckDiskSpace
  Push "Extracting glue.exe and shim.exe"
  Call LogLine
  File "${PAYLOAD_DIR}\glue.exe"
  File "${PAYLOAD_DIR}\shim.exe"
  ; The workspace (apps, shims, store, buckets, cache) is shared with
  ; Gluestick Desktop and lives in %USERPROFILE%\.glue, not beside the
  ; binaries. The marker is what the CLI recognises as an installation, since a
  ; bare glue.exe is exactly what a development build looks like.
  FileOpen $0 "$INSTDIR\.glue-install" w
  FileWrite $0 "Gluestick installation$\r$\nversion: ${PAYLOAD_VERSION}$\r$\n"
  FileClose $0

  ; Whether to seed the package proxy list below. config.json belongs to this
  ; installation, so an existing one always wins. The workspace copy is only a
  ; compatibility fallback for installs from the shared-workspace era: `root
  ; set --adopt` copies it into $INSTDIR right after this check, so seeding
  ; must not run and hide it.
  StrCpy $R1 1
  IfFileExists "$INSTDIR\config.json" glue_proxy_config_present
  IfFileExists "$PROFILE\.glue\config.json" glue_proxy_config_present
  Goto glue_proxy_config_missing
  glue_proxy_config_present:
    StrCpy $R1 0
  glue_proxy_config_missing:

  WriteUninstaller "$INSTDIR\Uninstall.exe"

  WriteRegStr HKCU "${UNINSTALL_KEY}" "InstallLocation" "$INSTDIR"
  WriteRegStr HKCU "${UNINSTALL_KEY}" "DisplayName" "${PRODUCT_NAME} ${PAYLOAD_VERSION} (${PAYLOAD_ARCH})"
  WriteRegStr HKCU "${UNINSTALL_KEY}" "DisplayVersion" "${PAYLOAD_VERSION}"
  WriteRegStr HKCU "${UNINSTALL_KEY}" "UninstallString" '"$INSTDIR\Uninstall.exe"'
  WriteRegDWORD HKCU "${UNINSTALL_KEY}" "NoModify" 1
  WriteRegDWORD HKCU "${UNINSTALL_KEY}" "NoRepair" 1
  Push "Installed glue.exe, shim.exe and installation metadata"
  Call LogLine

  ; Both products keep their data in %USERPROFILE%\.glue. `glue root set`
  ; writes data-root.txt (UTF-8, which the CLI and the shim read; NSIS's own
  ; FileWrite would emit UTF-16LE), creates the layout, and with --adopt moves
  ; data from an installation that still kept it beside the binaries.
  DetailPrint "Preparing the shared workspace..."
  Push "Workspace setup: starting"
  Call LogLine
  nsExec::ExecToLog '"$INSTDIR\glue.exe" tee "$INSTDIR\${INSTALL_LOG}" "$INSTDIR\glue.exe" -- root set "$PROFILE\.glue" --adopt'
  Pop $0
  Push "Workspace setup exit code: $0"
  Call LogLine
  ${If} $0 != "0"
    DetailPrint "Workspace setup failed (exit $0); Gluestick creates the workspace on first run."
  ${EndIf}

  ; claude-code is served from storage.googleapis.com, which some networks
  ; block. The user only has to fill in `proxy`; without it the list has no
  ; effect. `config set` writes config.json beside glue.exe (program-scoped,
  ; not in the workspace) and merges, so a config inherited from the old
  ; shared workspace keeps its other keys.
  ${If} $R1 == "1"
    DetailPrint "Seeding the package proxy list..."
    nsExec::ExecToLog '"$INSTDIR\glue.exe" tee "$INSTDIR\${INSTALL_LOG}" "$INSTDIR\glue.exe" -- config set proxy_packages claude-code'
    Pop $0
    Push "Proxy list setup exit code: $0"
    Call LogLine
  ${EndIf}

  ; Put the shared shims directory and this installation on the user PATH, as
  ; the CLI's path setup does.
  DetailPrint "Adding Gluestick to the user PATH..."
  Push "PATH setup: starting"
  Call LogLine
  nsExec::ExecToLog '"$INSTDIR\glue.exe" tee "$INSTDIR\${INSTALL_LOG}" "$INSTDIR\glue.exe" -- --root "$PROFILE\.glue" path setup'
  Pop $0
  Push "PATH setup exit code: $0"
  Call LogLine
  ${If} $0 != "0"
    DetailPrint "PATH setup failed (exit $0); add $PROFILE\.glue\shims and $INSTDIR to PATH manually."
  ${EndIf}
  Push "Gluestick CLI installation complete"
  Call LogLine
SectionEnd

; Selected by default, optional: the template downloads claude-code, codex and
; Magpie AI from their upstream hosts. A failure here must not roll back an
; otherwise working Gluestick, so it is only reported.
Section "Vibe Coding template" SecVibe
  DetailPrint "Installing selected Vibe Coding packages..."
  DetailPrint "This can take several minutes; progress is shown below."
  StrCpy $R0 ""
  ${If} $VibeClaude != 0
    StrCpy $R0 "$R0 ${VIBE_CLAUDE}"
  ${EndIf}
  ${If} $VibeCodex != 0
    StrCpy $R0 "$R0 ${VIBE_CODEX}"
  ${EndIf}
  ${If} $VibeMagpie != 0
    StrCpy $R0 "$R0 ${VIBE_MAGPIE}"
  ${EndIf}
  ${If} $R0 == ""
    DetailPrint "No Vibe Coding packages selected; skipping."
    Goto vibe_section_done
  ${EndIf}
  Push "Selected Vibe Coding packages:$R0"
  Call LogLine
  nsExec::ExecToLog '"$INSTDIR\glue.exe" tee "$INSTDIR\${INSTALL_LOG}" "$INSTDIR\glue.exe" -- --root "$PROFILE\.glue" install $R0'
  Pop $0
  Push "Vibe Coding packages exit code: $0"
  Call LogLine
  ${If} $0 != "0"
    DetailPrint "Vibe Coding package installation failed (exit $0)."
    IfSilent vibe_reported
    MessageBox MB_ICONEXCLAMATION|MB_OK "Gluestick is installed, but the selected Vibe Coding packages could not be installed.$\r$\n$\r$\nRun this later to retry:$\r$\n$\"$INSTDIR\glue.exe$\" install $R0$\r$\n$\r$\nSee the installation details for the error."
    vibe_reported:
  ${EndIf}
  vibe_section_done:
SectionEnd

!insertmacro MUI_FUNCTION_DESCRIPTION_BEGIN
  !insertmacro MUI_DESCRIPTION_TEXT ${SecMain} "Gluestick CLI and the shim launcher used by installed tools."
  !insertmacro MUI_DESCRIPTION_TEXT ${SecVibe} "Choose claude-code, codex and/or Magpie AI on the next page (network required; about 1 GB free)."
!insertmacro MUI_FUNCTION_DESCRIPTION_END

; Finish-page action: open the log in Notepad. Running `glue.exe` from here
; only flashes a console, and glue needs a terminal it can keep open.
Function ShowInstallLog
  ExecShell "open" "notepad.exe" "$\"$INSTDIR\${INSTALL_LOG}$\""
FunctionEnd

; Package selection page. It is skipped when the Vibe Coding section is not
; selected; all three packages start checked.
Function VibePageCreate
  SectionGetFlags ${SecVibe} $0
  IntOp $0 $0 & 1
  ${If} $0 == 0
    Abort
  ${EndIf}

  !insertmacro MUI_HEADER_TEXT "Vibe Coding packages" "Choose the tools to install."

  nsDialogs::Create 1018
  Pop $0
  ${If} $0 == error
    Abort
  ${EndIf}

  ${NSD_CreateCheckBox} 0 0 100% 12u "Claude Code (${VIBE_CLAUDE})"
  Pop $VibeClaudeCtl
  ${NSD_CreateCheckBox} 0 16u 100% 12u "Codex CLI (${VIBE_CODEX})"
  Pop $VibeCodexCtl
  ${NSD_CreateCheckBox} 0 32u 100% 12u "Magpie AI (${VIBE_MAGPIE})"
  Pop $VibeMagpieCtl
  ${NSD_CreateLabel} 0 54u 100% 12u "Packages are downloaded from the network."
  Pop $0
  ${NSD_CreateLabel} 0 68u 100% 12u "Installation can take several minutes."
  Pop $0

  ${NSD_SetState} $VibeClaudeCtl $VibeClaude
  ${NSD_SetState} $VibeCodexCtl $VibeCodex
  ${NSD_SetState} $VibeMagpieCtl $VibeMagpie

  nsDialogs::Show
FunctionEnd

Function VibePageLeave
  SectionGetFlags ${SecVibe} $0
  IntOp $0 $0 & 1
  ${If} $0 == 0
    Return
  ${EndIf}
  ${NSD_GetState} $VibeClaudeCtl $VibeClaude
  ${NSD_GetState} $VibeCodexCtl $VibeCodex
  ${NSD_GetState} $VibeMagpieCtl $VibeMagpie
FunctionEnd

; Append one line to $INSTDIR\install.log. The caller pushes the text.
Function LogLine
  Exch $0
  Push $1
  FileOpen $1 "$INSTDIR\${INSTALL_LOG}" a
  ; `a` does not always leave the handle at EOF in this NSIS build; without
  ; this seek, later lines overwrite the header and appear at the top.
  FileSeek $1 0 END
  FileWrite $1 "$0$\r$\n"
  FileClose $1
  Pop $1
  Pop $0
FunctionEnd

; Defined after both sections so ${SecVibe} is known at compile time. Aborts
; before any file is written when the target drive is too small; the template is
; counted only when the user kept that section selected.
Function CheckDiskSpace
  StrCpy $4 "$INSTDIR" 2
  StrCpy $4 "$4\"
  ${DriveSpace} "$4" "/D=F /S=M" $5
  ${If} ${Errors}
    Return
  ${EndIf}
  StrCpy $6 ${MIN_CLI_MB}
  SectionGetFlags ${SecVibe} $7
  IntOp $7 $7 & 1
  ${If} $7 != 0
    StrCpy $6 ${MIN_TEMPLATE_MB}
  ${EndIf}
  Push "Disk space: $5 MB free, $6 MB required"
  Call LogLine
  ${If} $5 < $6
    DetailPrint "Not enough disk space on $4: $5 MB free, $6 MB required."
    IfSilent disk_space_abort
    MessageBox MB_OK|MB_ICONSTOP "Not enough free disk space on $4.$\r$\n$\r$\nRequired: at least $6 MB (Gluestick CLI and, if selected, the Vibe Coding template).$\r$\nAvailable: $5 MB."
    disk_space_abort:
    SetErrorLevel 2
    Abort
  ${EndIf}
FunctionEnd

; Defined after both sections so ${SecVibe} is known at compile time.
Function .onInit
  ; All packages start selected. The custom page updates these in the GUI;
  ; silent installs keep all three.
  StrCpy $VibeClaude 1
  StrCpy $VibeCodex 1
  StrCpy $VibeMagpie 1

  ; Automated installs can skip the template download: /NOTEMPLATE=1
  ${GetOptions} $CMDLINE "/NOTEMPLATE=" $3
  ${IfNot} ${Errors}
    SectionSetFlags ${SecVibe} 0
  ${EndIf}

  ReadEnvStr $0 "PROCESSOR_ARCHITEW6432"
  ReadEnvStr $1 "PROCESSOR_ARCHITECTURE"
  StrCpy $2 $1
  ${If} $0 != ""
    StrCpy $2 $0
  ${EndIf}

  ${If} "${PAYLOAD_ARCH}" == "arm64"
    ${If} $2 != "ARM64"
      MessageBox MB_OK|MB_ICONSTOP "This ARM64 build requires Windows on ARM64."
      SetErrorLevel 2
      Abort
    ${EndIf}
  ${Else}
    ${If} $2 != "AMD64"
      ${If} $2 != "ARM64"
        MessageBox MB_OK|MB_ICONSTOP "This build requires 64-bit Windows."
        SetErrorLevel 2
        Abort
      ${EndIf}
    ${EndIf}
  ${EndIf}
FunctionEnd

Section "Uninstall"
  ; PATH first, while glue.exe still exists. No `--root`: the saved
  ; data-root.txt names the shared workspace, and `path remove` keeps its shims
  ; entry while the other Gluestick product is still installed.
  ExecWait '"$INSTDIR\glue.exe" path remove'

  Delete "$INSTDIR\install.log"
  Delete "$INSTDIR\glue.exe"
  Delete "$INSTDIR\shim.exe"
  Delete "$INSTDIR\data-root.txt"
  Delete "$INSTDIR\config.json"
  Delete "$INSTDIR\.glue-install"
  Delete "$INSTDIR\Uninstall.exe"

  ; The workspace (apps, shims, store, buckets, cache) lives in
  ; %USERPROFILE%\.glue and is shared with Gluestick Desktop, so it is never
  ; removed here. Any of these directories inside the installation means an
  ; upgrade whose data move did not finish: keep that data too and drop only the
  ; program files above.
  IfFileExists "$INSTDIR\apps" glue_uninstall_keep_data
  IfFileExists "$INSTDIR\persist" glue_uninstall_keep_data
  IfFileExists "$INSTDIR\store" glue_uninstall_keep_data
  Goto glue_uninstall_program_only
  glue_uninstall_keep_data:
    RMDir "$INSTDIR"
    Goto glue_uninstall_done
  glue_uninstall_program_only:
    RMDir /r "$INSTDIR"
    ; A running Uninstall.exe can stay locked on some systems even after NSIS
    ; copies itself to %TEMP%. Let a short-lived helper remove the leftover
    ; folder once this process has exited.
    ExecShell "open" "$SYSDIR\cmd.exe" "/c ping -n 3 127.0.0.1 >nul & rmdir /s /q $\"$INSTDIR$\"" SW_HIDE
  glue_uninstall_done:
  DeleteRegKey HKCU "${UNINSTALL_KEY}"

  ; Forget a saved root only when it points at *this* installation; a root the
  ; user re-pointed elsewhere with `glue root set` stays theirs.
  ReadRegStr $0 HKCU "Software\Gluestick" "DataRoot"
  StrCmp $0 "$INSTDIR" 0 glue_root_kept
  DeleteRegValue HKCU "Software\Gluestick" "DataRoot"
glue_root_kept:
  DeleteRegKey /ifempty HKCU "Software\Gluestick"
SectionEnd
