@echo off
title Stop wx_channels_download and restore network
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0Stop-WxChannelsDownload.ps1" %*
set "EXIT_CODE=%ERRORLEVEL%"
echo.
pause
exit /b %EXIT_CODE%
