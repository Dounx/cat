; Inno Setup 6 script for the Cat Windows desktop installer.
; Installs per-user under %LOCALAPPDATA%\Programs\Cat so no UAC elevation is
; required and Cat's built-in updater can replace files in place.
#ifndef Version
  #define Version "0.1.0"
#endif

[Setup]
AppId={{7B6E8E5A-4E1B-4C7D-9D31-8C9F6E2A1B40}
AppName=Cat
AppVersion={#Version}
AppPublisher=Dounx
AppPublisherURL=https://github.com/Dounx/cat
AppSupportURL=https://github.com/Dounx/cat/issues
DefaultDirName={autopf}\Cat
DefaultGroupName=Cat
PrivilegesRequired=lowest
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
DisableProgramGroupPage=yes
OutputDir=..\..\bin
OutputBaseFilename=Cat-setup
Compression=lzma2/normal
SolidCompression=yes
WizardStyle=modern
CloseApplications=yes
UninstallDisplayIcon={app}\Cat.exe
#if FileExists("..\..\bin\cat.ico")
SetupIconFile=..\..\bin\cat.ico
#endif

[Languages]
Name: "english"; MessagesFile: "compiler:Default.isl"

[Tasks]
Name: "desktopicon"; Description: "{cm:CreateDesktopIcon}"; GroupDescription: "{cm:AdditionalIcons}"; Flags: unchecked

[Files]
Source: "..\..\bin\Cat-windows\*"; DestDir: "{app}"; Flags: ignoreversion recursesubdirs createallsubdirs

[Icons]
Name: "{autoprograms}\Cat"; Filename: "{app}\Cat.exe"
Name: "{autodesktop}\Cat"; Filename: "{app}\Cat.exe"; Tasks: desktopicon

[Run]
Filename: "{app}\Cat.exe"; Description: "{cm:LaunchProgram,Cat}"; Flags: nowait postinstall skipifsilent
