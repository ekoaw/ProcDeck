@echo off
setlocal
title ProcDeck
rem php.exe is read from the "php =" line of procdeck.ini.
set "PHP="
for /f "usebackq eol=; tokens=1,* delims== " %%A in ("%~dp0procdeck.ini") do if /i "%%A"=="php" set "PHP=%%~B"
if not defined PHP set "PHP=php.exe"
"%PHP%" -v >nul 2>&1
if errorlevel 1 (
    echo PHP not found: %PHP%
    echo Set the "php =" line in %~dp0procdeck.ini to the location of php.exe, for example C:\php\php.exe
    pause
    exit /b 1
)
"%PHP%" "%~dp0manager.php"
if errorlevel 1 pause
