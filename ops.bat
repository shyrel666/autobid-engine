@echo off
chcp 65001 >nul 2>&1
rem AutoBid 运维命令入口：读取 .env.local.bat 后运行 bin\autobid-ops.exe。用法见 docs\OPERATIONS.md。
rem   ops.bat check                       只读检查，升级或停机前运行
rem   ops.bat backup --out D:\backup\x    备份数据库和数据目录（目录必须为空或不存在）
rem   ops.bat verify --dir D:\backup\x    校验备份
setlocal
cd /d "%~dp0"
if exist ".env.local.bat" call ".env.local.bat"
if not exist "bin\autobid-ops.exe" (
    echo [autobid] 未找到 bin\autobid-ops.exe，请先运行 start.bat --build-only。 1>&2
    exit /b 1
)
"bin\autobid-ops.exe" %*
exit /b %ERRORLEVEL%
