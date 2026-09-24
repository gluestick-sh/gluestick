; Glue Windows installer (NSIS)
;   makensis /DPAYLOAD_VERSION=0.1.10 /DPAYLOAD_ARCH=amd64 Glue.nsi

!include "MUI2.nsh"
!include "LogicLib.nsh"
!include "FileFunc.nsh"

!ifndef PAYLOAD_VERSION
  !define PAYLOAD_VERSION "0.0.0-dev"
!endif
!ifndef PAYLOAD_ARCH
  !define PAYLOAD_ARCH "amd64"
!endif

!define PRODUCT_NAME "Glue"
!define PRODUCT_PUBLISHER "gluestick.sh"
!define PRODUCT_URL "https://gluestick.sh/"
!define PAYLOAD_DIR "payload\${PAYLOAD_ARCH}"
!define UNINST_KEY "Software\Microsoft\Windows\CurrentVersion\Uninstall\Glue-${PAYLOAD_ARCH}"

Name "${PRODUCT_NAME} ${PAYLOAD_VERSION} (${PAYLOAD_ARCH})"
OutFile "output\GlueSetup-${PAYLOAD_ARCH}.exe"
InstallDir "$PROFILE\.glue"
InstallDirRegKey HKCU "${UNINST_KEY}" "InstallLocation"
RequestExecutionLevel user
ShowInstDetails show
ShowUninstDetails show

!define MUI_ABORTWARNING
!define MUI_FINISHPAGE_RUN
!define MUI_FINISHPAGE_RUN_TEXT "Open Glue install folder"
!define MUI_FINISHPAGE_RUN_FUNCTION LaunchInstallDir

!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH

!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES

!insertmacro MUI_LANGUAGE "English"

Function LaunchInstallDir
  ExecShell "open" "$INSTDIR"
FunctionEnd

Function .onInit
  ReadRegStr $0 HKCU "${UNINST_KEY}" "InstallLocation"
  ${If} $0 != ""
    StrCpy $INSTDIR $0
  ${EndIf}

  StrCmp "${PAYLOAD_ARCH}" "arm64" 0 archOk
    ; $$ so NSIS does not eat PowerShell $env:... (warning 6000 otherwise)
    nsExec::ExecToStack 'powershell.exe -NoProfile -Command "if ($$env:PROCESSOR_ARCHITECTURE -match ''ARM64'' -or $$env:PROCESSOR_ARCHITEW6432 -match ''ARM64'') { exit 0 } else { exit 1 }"'
    Pop $0
    Pop $1
    IntCmp $0 0 archOk arm64Bad arm64Bad
arm64Bad:
    MessageBox MB_OK|MB_ICONSTOP "GlueSetup-${PAYLOAD_ARCH}.exe requires Windows on ARM64."
    Abort
archOk:
FunctionEnd

Section "Glue" SecMain
  SectionIn RO
  SetOutPath "$INSTDIR"

  File "${PAYLOAD_DIR}\glue.exe"
  File "${PAYLOAD_DIR}\shim.exe"

  SetOutPath "$INSTDIR\bin"
  File "${PAYLOAD_DIR}\bin\7z.exe"
  File "${PAYLOAD_DIR}\bin\7z.dll"
  File "${PAYLOAD_DIR}\bin\mingit.zip"

  SetOutPath "$INSTDIR"
  File "install-finish.ps1"
  File "uninstall-finish.ps1"

  DetailPrint "Configuring Glue (MinGit, PATH, shims)..."
  nsExec::ExecToLog 'powershell.exe -NoProfile -ExecutionPolicy Bypass -Sta -File "$INSTDIR\install-finish.ps1" -GlueRoot "$INSTDIR" -Arch "${PAYLOAD_ARCH}"'
  Pop $0
  ${If} $0 != 0
    MessageBox MB_OK|MB_ICONSTOP "Glue post-install failed (exit $0).$\nCheck antivirus and retry."
    Abort
  ${EndIf}

  Delete "$INSTDIR\install-finish.ps1"

  WriteUninstaller "$INSTDIR\Uninstall.exe"
  WriteRegStr HKCU "${UNINST_KEY}" "InstallLocation" "$INSTDIR"
  WriteRegStr HKCU "${UNINST_KEY}" "DisplayName" "${PRODUCT_NAME} (${PAYLOAD_ARCH})"
  WriteRegStr HKCU "${UNINST_KEY}" "DisplayVersion" "${PAYLOAD_VERSION}"
  WriteRegStr HKCU "${UNINST_KEY}" "Publisher" "${PRODUCT_PUBLISHER}"
  WriteRegStr HKCU "${UNINST_KEY}" "URLInfoAbout" "${PRODUCT_URL}"
  WriteRegStr HKCU "${UNINST_KEY}" "UninstallString" '"$INSTDIR\Uninstall.exe"'
SectionEnd

Section "Uninstall"
  IfFileExists "$INSTDIR\uninstall-finish.ps1" 0 +3
    nsExec::ExecToLog 'powershell.exe -NoProfile -ExecutionPolicy Bypass -Sta -File "$INSTDIR\uninstall-finish.ps1" -GlueRoot "$INSTDIR"'
    Delete "$INSTDIR\uninstall-finish.ps1"

  Delete "$INSTDIR\glue.exe"
  Delete "$INSTDIR\shim.exe"
  Delete "$INSTDIR\Uninstall.exe"
  Delete "$INSTDIR\bin\7z.exe"
  Delete "$INSTDIR\bin\7z.dll"
  Delete "$INSTDIR\bin\mingit.zip"
  RMDir /r "$INSTDIR\bin\git"
  RMDir "$INSTDIR\bin"
  RMDir /r "$INSTDIR\shims"
  RMDir /r "$INSTDIR\shims-meta"

  DeleteRegKey HKCU "${UNINST_KEY}"
  RMDir "$INSTDIR"
SectionEnd
