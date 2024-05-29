import { Module } from '@nestjs/common';
import { FileService } from './file.service';
import { FileAdapterService } from './file.adapter.service';
import { FileController } from './file.controller';

@Module({
  imports: [],
  providers: [FileService, FileAdapterService],
  exports: [FileService],
  controllers: [FileController],
})
export class FileModule {}
