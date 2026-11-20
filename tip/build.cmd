@echo off
rem Builds internal\tip\kototype_tip_x64.dll and kototype_tip_x86.dll with MSVC.
rem Each distinct kototype_tip.cpp gets its own deterministic CLSID, profile GUID
rem and window class so TSF hot-loads updated DLLs into already-running apps
rem without colliding with an earlier copy mapped in the same process.
rem Rebuild after changing kototype_tip.cpp.
setlocal
for /f "usebackq tokens=*" %%i in (`"%ProgramFiles(x86)%\Microsoft Visual Studio\Installer\vswhere.exe" -latest -property installationPath`) do set VS=%%i
if not defined VS (echo Visual Studio with C++ tools not found & exit /b 1)
cd /d "%~dp0"
powershell -NoProfile -ExecutionPolicy Bypass -File gen_id.ps1 || exit /b 1
for %%a in (x64 x86) do (
	if not exist obj\%%a mkdir obj\%%a
	rem /Brepro: the same source gives the same bytes, so the installed version only changes with the code.
	rem No C runtime (/Zl /NODEFAULTLIB, no /GS, exceptions or RTTI): DllMain is the entry point.
	cmd /c ""%VS%\VC\Auxiliary\Build\vcvarsall.bat" %%a >nul && cl /nologo /Brepro /O1 /W4 /GS- /GR- /EHs-c- /Zl /utf-8 /DUNICODE /D_UNICODE /LD /Foobj\%%a\ kototype_tip.cpp /link /Brepro /NODEFAULTLIB /ENTRY:DllMain /DEF:kototype_tip.def /IMPLIB:obj\%%a\kototype_tip.lib kernel32.lib ole32.lib oleaut32.lib uuid.lib user32.lib advapi32.lib /OUT:obj\%%a\kototype_tip.dll" || exit /b 1
	copy /y obj\%%a\kototype_tip.dll ..\internal\tip\kototype_tip_%%a.dll >nul || exit /b 1
)
