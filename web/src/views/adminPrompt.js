// A minimal token prompt for the shared admin gate (see server/adminauth.go).
// This is deliberately not a real login form: there's one shared secret, not
// an account, so a plain browser prompt is an honest match for what it is.
export async function promptForAdminToken() {
  const token = window.prompt(
    "This server requires an admin token to manage projects.\nEnter PLAYGROUND_ADMIN_TOKEN:",
  );
  return token?.trim() || null;
}
