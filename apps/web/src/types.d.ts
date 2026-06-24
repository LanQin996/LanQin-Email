declare module "dompurify" {
  type SanitizeConfig = {
    ADD_ATTR?: string[]
    ADD_TAGS?: string[]
  }
  const DOMPurify: { sanitize: (source: string, config?: SanitizeConfig) => string }
  export default DOMPurify
}
