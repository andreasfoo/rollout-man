# Language Policy

## English Only

Regardless of the language used in the user's input, **always respond in English**.

This applies to:

* Normal conversational responses
* Explanations and reasoning summaries
* Error messages and status messages
* Commit messages
* Documentation
* Markdown files
* Generated reports
* Slide content
* Comments in newly generated code
* Strings in generated configuration or data files, when the content is user-facing

### Rules

1. **Never switch the response language based on the user's input language.**

   * Chinese input → English response
   * Japanese input → English response
   * Korean input → English response
   * Mixed-language input → English response
   * English input → English response

2. **All newly created or modified documentation must be written in English.**

3. **All generated presentation content must be written in English**, including:

   * Slide titles
   * Bullet points
   * Captions
   * Diagram labels
   * Tables
   * Speaker notes

4. **All newly generated code comments should be in English.**

5. Preserve technical identifiers exactly as required by the source or codebase:

   * API names
   * Function names
   * Variable names
   * Class names
   * CLI commands
   * File paths
   * URLs
   * Package names
   * Product names
   * Proper nouns

6. When quoting user-provided text, preserve the original language **only when necessary to accurately reproduce the quote**. Any explanation surrounding the quote must still be in English.

7. Do not translate source code, commands, identifiers, or technical syntax merely for the sake of enforcing English.

## Examples

### User input

> 帮我解释一下这个 bug

### Response

> The bug is caused by ...

### User input

> 帮我写一个中文 README

### Response behavior

Create the README in **English**, because all generated documentation must follow this language policy.

### User input

> 用中文写一个 Marp presentation

### Response behavior

Generate the Marp presentation entirely in **English**, including slide titles, body text, diagrams, and captions.

## Priority

This language policy applies unless the user explicitly asks for a direct translation or explicitly requires preserving the original language in a specific artifact.

When there is a conflict:

1. Follow explicit user instructions for direct translation or exact quotation.
2. Otherwise, use English.
