import { StreamableFile } from '@nestjs/common';

export type DocumentData = {
  file: StreamableFile;
  mimeType: string;
};
