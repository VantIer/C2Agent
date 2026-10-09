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

echo ^>^> vet remote-go
pushd remote\remote-go
go vet ./... || (popd & exit /b 1)
popd

echo ^>^> test control end
go test ./... || exit /b 1

echo ^>^> test remote-go
pushd remote\remote-go
go test ./... || (popd & exit /b 1)
popd

where gcc >nul 2>nul
if errorlevel 1 goto no_gcc
echo ^>^> test remote-c self-tests
pushd remote\remote-c
gcc -O2 -Wall -Wextra -o "%TEMP%\c2a_crypto_selftest.exe" tests\crypto_selftest.c -lws2_32 || goto test_fail
"%TEMP%\c2a_crypto_selftest.exe" || goto test_fail
gcc -O2 -Wall -Wextra -o "%TEMP%\c2a_edit_selftest.exe" tests\edit_selftest.c protocol.c -lws2_32 || goto test_fail
"%TEMP%\c2a_edit_selftest.exe" || goto test_fail
del "%TEMP%\c2a_crypto_selftest.exe" "%TEMP%\c2a_edit_selftest.exe" >nul 2>nul
popd
goto py_tests

:no_gcc
echo ^>^> remote-c self-tests skipped ^(no gcc^)

:py_tests
where python >nul 2>nul
if errorlevel 1 goto no_py
echo ^>^> test remote-py
python remote\common\test_crypto.py || goto test_fail
python remote\remote-py\test_local_executor.py || goto test_fail
goto tests_done

:no_py
echo ^>^> remote-py tests skipped ^(no python^)

:tests_done

call :build windows amd64 .exe
call :build linux   amd64
call :build linux   386
call :build linux   arm64
call :build windows 386   .exe
goto :eof

:test_fail
exit /b 1

:build
set GOOS=%~1
set GOARCH=%~2
set EXT=%~3
echo ^>^> control    %GOOS%/%GOARCH%
go build -trimpath -tags "%TAGS%" -ldflags "%LDFLAGS%" -o "dist\control\c2agent_%GOOS%_%GOARCH%%EXT%" ./cmd/c2agent || exit /b 1
echo ^>^> remote-go  %GOOS%/%GOARCH%
pushd remote\remote-go
go build -trimpath -tags "%TAGS%" -ldflags "%LDFLAGS%" -o "..\..\dist\remote-go\c2agent_remote_%GOOS%_%GOARCH%%EXT%" . || (popd & exit /b 1)
popd
goto :eof
