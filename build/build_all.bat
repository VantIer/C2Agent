@echo off
REM Build the C2 control end and the Go controlled end for multiple platforms.
REM Fully static, CGO disabled, zero external runtime dependencies.
setlocal
cd /d "%~dp0.."

set CGO_ENABLED=0
set TAGS=netgo,osusergo
set LDFLAGS=-s -w
if not exist dist\control mkdir dist\control
if not exist dist\remote-go mkdir dist\remote-go

call :build windows amd64 .exe
call :build linux   amd64
call :build linux   386
call :build linux   arm64
call :build windows 386   .exe
goto :eof

:build
set GOOS=%~1
set GOARCH=%~2
set EXT=%~3
echo ^>^> control    %GOOS%/%GOARCH%
go build -trimpath -tags "%TAGS%" -ldflags "%LDFLAGS%" -o "dist\control\c2agent_%GOOS%_%GOARCH%%EXT%" ./cmd/c2agent || exit /b 1
echo ^>^> remote-go  %GOOS%/%GOARCH%
pushd remote\remote-go
go build -trimpath -tags "%TAGS%" -ldflags "%LDFLAGS%" -o "..\..\dist\remote-go\c2a_remote_%GOOS%_%GOARCH%%EXT%" . || (popd & exit /b 1)
popd
goto :eof
