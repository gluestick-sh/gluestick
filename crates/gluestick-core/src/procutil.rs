//! Process helpers.
//!
//! The window-hiding flag for child processes (on Windows the engine shells out
//! to tools like `7z.exe` and `git`, and without `CREATE_NO_WINDOW` each call
//! would flash a console window), plus the path form `cmd` built-ins require.

use std::path::Path;
use std::process::Command;

/// Keep the console window hidden when shelling out to `cmd`.
///
/// A no-op off Windows; there the flag does not exist (and no console would be
/// spawned for a GUI parent anyway).
pub fn hide_window(cmd: &mut Command) {
    #[cfg(windows)]
    {
        use std::os::windows::process::CommandExt;
        const CREATE_NO_WINDOW: u32 = 0x0800_0000;
        cmd.creation_flags(CREATE_NO_WINDOW);
    }
    #[cfg(not(windows))]
    {
        let _ = cmd;
    }
}

/// A path as it must appear on a `cmd` command line: backslashes only.
///
/// `Path::join("apps/pkg/1.0.0")` keeps the forward slashes it was handed (and a
/// config-supplied `--root` may use them), while the `cmd` built-ins used here
/// — `mklink /J`, `rmdir` — treat any `/` as an option prefix. Without this, a
/// persist entry like `{"foo/bar": …}` fails with `Invalid switch - "bar"` even
/// though every Windows file API happily accepts the same path.
pub fn cmd_path_arg(path: &Path) -> String {
    path.to_string_lossy().replace('/', "\\")
}
