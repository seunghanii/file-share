@echo off
rem Go(https://go.dev/dl/) 설치 후 이 파일을 실행하면 파일공유.exe를 다시 만듭니다.
cd /d "%~dp0"
go build -trimpath -ldflags "-H windowsgui -s -w" -o 파일공유.exe .
pause
