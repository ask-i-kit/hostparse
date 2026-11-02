@echo off
rem Build hostparse.exe (CLI) and hostparse-gui.exe (GUI, no console window)
cd /d "%~dp0"
go build -trimpath -ldflags "-s -w" -o hostparse.exe ./cmd/hostparse || exit /b 1
go build -trimpath -ldflags "-s -w -H windowsgui" -o hostparse-gui.exe ./cmd/hostparse-gui || exit /b 1
echo Built: hostparse.exe / hostparse-gui.exe
