import js from "@eslint/js"
import tseslint from "typescript-eslint"

export default tseslint.config(
  { ignores: ["dist/**", "node_modules/**", "test/**"] },
  js.configs.recommended,
  ...tseslint.configs.recommended,
  {
    // The SDK is a wire client: JSON bodies are decoded as `any` on purpose,
    // because the contract is the server's openapi.yaml, not a local type. The
    // rule is off deliberately, not silenced to get green.
    rules: { "@typescript-eslint/no-explicit-any": "off" },
  },
)
