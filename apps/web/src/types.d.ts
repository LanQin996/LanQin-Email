declare module "dompurify" {
  type SanitizeConfig = {
    ADD_ATTR?: string[]
    ADD_TAGS?: string[]
    WHOLE_DOCUMENT?: boolean
  }
  const DOMPurify: { sanitize: (source: string, config?: SanitizeConfig) => string }
  export default DOMPurify
}
