# Spinifex UI

Full-stack application for managing Spinifex AWS compatible products (EC2, S3)

## Development Setup

Note: This is not needed for actually building spinifex-ui and running it. We commit the assets so that when you build spinifex, it will include spinifex-ui without you needing to install node and build frontend from source.

1. **Install Node.js using nvm**

This project uses Node.js 24.12.0 (specified in `.nvmrc`).

   ```sh
   cd frontend
   # Install nvm if you don't have it
   curl -o- https://raw.githubusercontent.com/nvm-sh/nvm/v0.40.3/install.sh | bash

   # Restart source nvm
   \. "$HOME/.nvm/nvm.sh"

   # Install and use the project's Node.js version
   nvm install
   nvm use
   ```

The `.nvmrc` file in the project root ensures you use the correct Node.js version. Run `nvm use` whenever you enter this directory.

2. **Enable Corepack**

   ```sh
   corepack enable
   ```

3. **Install Dependencies**

   ```sh
   pnpm install
   ```

4. **Install Agent Browser**
   ```sh
   npm install -g agent-browser
   agent-browser install
   ```

5. **Accept Certs In Browser**

If you have added the CA to your machine you do not need to do this. But if you are sshd into a spinifex machine and want to view the ui, go to [https://localhost:9999](https://localhost:9999) and [https://localhost:8443](https://localhost:8443) and accept the certificates

6. **Launch Server**

For development use:

   ```sh
   cd frontend
   pnpm dev
   ```
